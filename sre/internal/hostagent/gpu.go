package hostagent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"clusterheart/sre/internal/contract"
)

// GPU temperature thresholds (°C). Data-center GPUs start slowing themselves
// down in the high 80s.
const (
	gpuWarnTempC, gpuBadTempC = 85.0, 92.0
	gpuRecheck                = time.Hour // how often to look for newly installed GPUs or drivers
)

// nvidiaQuery asks nvidia-smi for one CSV line per GPU (see parseNvidiaSMI).
var nvidiaQuery = []string{"--query-gpu=index,uuid,name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw,power.limit", "--format=csv,noheader,nounits"}

const (
	nvidiaStreamEvery = 5 * time.Second  // sample interval of the long-running nvidia-smi (several per snapshot)
	nvidiaRestart     = 5 * time.Minute  // wait before restarting one that exited
	nvidiaOneShotMax  = 30 * time.Second // nvidia-smi can be slow without persistence mode
)

// gpuReader reads GPUs: NVIDIA through nvidia-smi, AMD through the amdgpu
// driver's sysfs files. Both are read-only.
type gpuReader struct {
	run      runFunc
	start    startFunc // nil: run nvidia-smi once per sample instead of streaming
	sysfs    string
	lookPath func(string) (string, error)

	checkedAt time.Time
	nvidiaSMI string // path to nvidia-smi, "" when not installed
	nvidiaHW  bool   // an NVIDIA display/3D controller is on the PCI bus
	stream    nvidiaStream
}

// nvidiaStream keeps one "nvidia-smi --query-gpu ... -l 15" running, so the
// driver stays initialised between samples: starting nvidia-smi for every
// sample can cost seconds of CPU on GPUs without persistence mode. If it
// exits, or prints nothing (e.g. buffered output), the reader falls back to
// running nvidia-smi once per sample.
type nvidiaStream struct {
	mu        sync.Mutex
	running   bool
	unusable  bool
	started   time.Time
	stoppedAt time.Time
	last      time.Time // last GPU line
	gpus      map[int]contract.HostGPU
	seen      map[int]time.Time
	// Utilisation samples since the last read, per GPU, so a snapshot reports
	// the average and peak over its whole window, not one instant.
	utilSum  map[int]float64
	utilN    map[int]int
	utilPeak map[int]float64
	cancel   context.CancelFunc
}

func newGPUReader(run runFunc, start startFunc, sysfs string) *gpuReader {
	return &gpuReader{run: run, start: start, sysfs: sysfs, lookPath: exec.LookPath}
}

// read returns the GPUs and problems reading them (driver down, tools missing).
func (g *gpuReader) read(ctx context.Context, now time.Time) ([]contract.HostGPU, []contract.HostProblem) {
	if g.checkedAt.IsZero() || now.Sub(g.checkedAt) >= gpuRecheck {
		g.checkedAt = now
		g.nvidiaSMI = ""
		for _, p := range []string{"nvidia-smi", "/usr/bin/nvidia-smi", "/usr/local/nvidia/bin/nvidia-smi"} {
			if path, err := g.lookPath(p); err == nil {
				g.nvidiaSMI = path
				break
			}
		}
		g.nvidiaHW = pciHasVendor(filepath.Join(g.sysfs, "bus", "pci", "devices"), "0x10de")
	}
	var gpus []contract.HostGPU
	var probs []contract.HostProblem
	switch {
	case g.nvidiaSMI != "":
		if streamed, ok := g.streamed(ctx, now); ok {
			gpus = append(gpus, streamed...)
			break
		}
		cctx, cancel := context.WithTimeout(ctx, nvidiaOneShotMax)
		out, err := g.run(cctx, g.nvidiaSMI, nvidiaQuery...)
		cancel()
		if err != nil {
			msg := firstLine(out, err)
			// "No devices were found" on a machine without an NVIDIA card is normal.
			if g.nvidiaHW || !strings.Contains(msg, "No devices were found") {
				probs = append(probs, contract.HostProblem{Type: "GPUUnavailable", Severity: "high", Message: "nvidia-smi failed: " + msg})
			}
		} else {
			gpus = append(gpus, parseNvidiaSMI(out)...)
		}
	case g.nvidiaHW:
		probs = append(probs, contract.HostProblem{Type: "GPUUnavailable", Severity: "medium",
			Message: "an NVIDIA GPU is installed but nvidia-smi (the NVIDIA driver tools) is not, so its usage cannot be read"})
	}
	amd := amdGPUs(filepath.Join(g.sysfs, "class", "drm"))
	for i := range amd {
		amd[i].Index = len(gpus)
		gpus = append(gpus, amd[i])
	}
	return gpus, probs
}

// streamed returns the latest GPUs from the long-running nvidia-smi, starting
// it when needed. ok is false when the caller should run nvidia-smi once.
func (g *gpuReader) streamed(ctx context.Context, now time.Time) ([]contract.HostGPU, bool) {
	if g.start == nil {
		return nil, false
	}
	s := &g.stream
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unusable {
		return nil, false
	}
	if !s.running {
		if !s.stoppedAt.IsZero() && now.Sub(s.stoppedAt) < nvidiaRestart {
			return nil, false
		}
		sctx, cancel := context.WithCancel(ctx)
		args := append(append([]string(nil), nvidiaQuery...), "-l", strconv.Itoa(int(nvidiaStreamEvery.Seconds())))
		r, err := g.start(sctx, g.nvidiaSMI, args...)
		if err != nil {
			cancel()
			s.unusable = true
			return nil, false
		}
		s.running, s.started, s.cancel = true, now, cancel
		s.last, s.gpus, s.seen = time.Time{}, map[int]contract.HostGPU{}, map[int]time.Time{}
		s.utilSum, s.utilN, s.utilPeak = map[int]float64{}, map[int]int{}, map[int]float64{}
		go s.consume(r)
		return nil, false // nothing read yet
	}
	stale := 3 * nvidiaStreamEvery
	if s.last.IsZero() || now.Sub(s.last) > stale {
		if now.Sub(s.started) > stale { // silent: output buffered or nvidia-smi hung
			s.cancel()
			s.running, s.unusable = false, true
		}
		return nil, false
	}
	var out []contract.HostGPU
	for idx, at := range s.seen {
		if now.Sub(at) <= 2*nvidiaStreamEvery {
			g := s.gpus[idx]
			if n := s.utilN[idx]; n > 1 {
				g.UtilPercent = round1(s.utilSum[idx] / float64(n))
				g.UtilPeakPercent = s.utilPeak[idx]
			}
			delete(s.utilSum, idx)
			delete(s.utilN, idx)
			delete(s.utilPeak, idx)
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, len(out) > 0
}

func (s *nvidiaStream) consume(r io.ReadCloser) {
	defer r.Close()
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		gs := parseNvidiaSMI(sc.Bytes())
		if len(gs) == 0 {
			continue
		}
		at := time.Now()
		s.mu.Lock()
		for _, g := range gs {
			s.gpus[g.Index], s.seen[g.Index] = g, at
			s.utilSum[g.Index] += g.UtilPercent
			s.utilN[g.Index]++
			if g.UtilPercent > s.utilPeak[g.Index] {
				s.utilPeak[g.Index] = g.UtilPercent
			}
		}
		s.last = at
		s.mu.Unlock()
	}
	s.mu.Lock()
	if s.running {
		s.running, s.stoppedAt = false, time.Now()
		s.cancel()
	}
	s.mu.Unlock()
}

func firstLine(out []byte, err error) string {
	msg := strings.TrimSpace(string(out))
	var ee *exec.ExitError
	if msg == "" && errors.As(err, &ee) {
		msg = strings.TrimSpace(string(ee.Stderr))
	}
	if msg == "" && err != nil {
		msg = err.Error()
	}
	msg, _, _ = strings.Cut(msg, "\n")
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return msg
}

// gpuNum parses a CSV value; "[N/A]" and "[Not Supported]" read as 0.
func gpuNum(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}

// parseNvidiaSMI reads
// "index, uuid, name, util %, mem used MiB, mem total MiB, temp C, power W, power limit W"
// lines. A name containing commas is rejoined.
func parseNvidiaSMI(out []byte) []contract.HostGPU {
	var gpus []contract.HostGPU
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Split(l, ",")
		if len(f) < 9 {
			continue
		}
		n := len(f)
		name := strings.TrimSpace(strings.Join(f[2:n-6], ","))
		idx, err := strconv.Atoi(strings.TrimSpace(f[0]))
		if err != nil {
			continue
		}
		gpus = append(gpus, contract.HostGPU{
			Index: idx, Vendor: "nvidia", Name: name, UUID: strings.TrimSpace(f[1]),
			UtilPercent: gpuNum(f[n-6]), MemUsedMi: int64(gpuNum(f[n-5])), MemTotalMi: int64(gpuNum(f[n-4])),
			TempC: gpuNum(f[n-3]), PowerW: round1(gpuNum(f[n-2])), PowerLimitW: round1(gpuNum(f[n-1])),
		})
	}
	return gpus
}

// pciHasVendor reports whether a display or 3D controller (PCI class 0x03xxxx)
// from vendor is present.
func pciHasVendor(devices, vendor string) bool {
	entries, err := os.ReadDir(devices)
	if err != nil {
		return false
	}
	for _, e := range entries {
		dir := filepath.Join(devices, e.Name())
		if readFirst(filepath.Join(dir, "vendor")) == vendor && strings.HasPrefix(readFirst(filepath.Join(dir, "class")), "0x03") {
			return true
		}
	}
	return false
}

var drmCard = regexp.MustCompile(`^card[0-9]+$`)

// amdGPUs reads AMD GPUs from the amdgpu driver's sysfs files.
func amdGPUs(drm string) []contract.HostGPU {
	entries, err := os.ReadDir(drm)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if drmCard.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Slice(names, func(i, j int) bool {
		a, _ := strconv.Atoi(strings.TrimPrefix(names[i], "card"))
		b, _ := strconv.Atoi(strings.TrimPrefix(names[j], "card"))
		return a < b
	})
	var gpus []contract.HostGPU
	for _, n := range names {
		dev := filepath.Join(drm, n, "device")
		if readFirst(filepath.Join(dev, "vendor")) != "0x1002" {
			continue
		}
		busy := readFirst(filepath.Join(dev, "gpu_busy_percent"))
		if busy == "" {
			continue // not an amdgpu-driven device
		}
		g := contract.HostGPU{Vendor: "amd", Name: readFirst(filepath.Join(dev, "product_name")), UUID: readFirst(filepath.Join(dev, "unique_id")),
			UtilPercent: gpuNum(busy)}
		if g.Name == "" {
			g.Name = "AMD GPU (" + n + ")"
		}
		used, _ := strconv.ParseInt(readFirst(filepath.Join(dev, "mem_info_vram_used")), 10, 64)
		total, _ := strconv.ParseInt(readFirst(filepath.Join(dev, "mem_info_vram_total")), 10, 64)
		g.MemUsedMi, g.MemTotalMi = used>>20, total>>20
		if hw, _ := filepath.Glob(filepath.Join(dev, "hwmon", "hwmon*")); len(hw) > 0 {
			g.TempC = gpuNum(readFirst(filepath.Join(hw[0], "temp1_input"))) / 1000
			p := readFirst(filepath.Join(hw[0], "power1_average"))
			if p == "" {
				p = readFirst(filepath.Join(hw[0], "power1_input"))
			}
			g.PowerW = round1(gpuNum(p) / 1e6)
			g.PowerLimitW = round1(gpuNum(readFirst(filepath.Join(hw[0], "power1_cap"))) / 1e6)
		}
		gpus = append(gpus, g)
	}
	return gpus
}

// gpuProblems flags hot GPUs.
func gpuProblems(gpus []contract.HostGPU) []contract.HostProblem {
	var out []contract.HostProblem
	for _, g := range gpus {
		switch {
		case g.TempC >= gpuBadTempC:
			out = append(out, contract.HostProblem{Type: "GPUHot", Severity: "high", Message: fmt.Sprintf("GPU %d (%s) is at %.0f°C", g.Index, g.Name, g.TempC)})
		case g.TempC >= gpuWarnTempC:
			out = append(out, contract.HostProblem{Type: "GPUHot", Severity: "medium", Message: fmt.Sprintf("GPU %d (%s) is at %.0f°C", g.Index, g.Name, g.TempC)})
		}
	}
	return out
}
