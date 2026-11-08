package hostagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseNvidiaSMI(t *testing.T) {
	out := "0, GPU-1111, NVIDIA A10G, 87, 20000, 23028, 71, 180.52, 300.00\n" +
		"1, GPU-2222, NVIDIA H100 80GB HBM3, 0, 4, 81559, 33, [N/A], [Not Supported]\n" +
		"2, GPU-3333, Odd, Name, With Commas, 5, 10, 100, 40, 50.0, 70.0\n" +
		"garbage\n"
	g := parseNvidiaSMI([]byte(out))
	if len(g) != 3 {
		t.Fatalf("gpus: %+v", g)
	}
	if g[0].Name != "NVIDIA A10G" || g[0].UUID != "GPU-1111" || g[0].UtilPercent != 87 || g[0].MemUsedMi != 20000 || g[0].MemTotalMi != 23028 ||
		g[0].TempC != 71 || g[0].PowerW != 180.5 || g[0].PowerLimitW != 300 || g[0].Vendor != "nvidia" {
		t.Fatalf("gpu 0: %+v", g[0])
	}
	if g[1].PowerW != 0 || g[1].PowerLimitW != 0 || g[1].MemTotalMi != 81559 {
		t.Fatalf("N/A values: %+v", g[1])
	}
	if g[2].Name != "Odd, Name, With Commas" || g[2].UtilPercent != 5 {
		t.Fatalf("comma name: %+v", g[2])
	}
}

func TestAMDGPUFromSysfs(t *testing.T) {
	root := t.TempDir()
	dev := filepath.Join(root, "class", "drm", "card1", "device")
	write(t, filepath.Join(dev, "vendor"), "0x1002\n")
	write(t, filepath.Join(dev, "gpu_busy_percent"), "42\n")
	write(t, filepath.Join(dev, "mem_info_vram_used"), "1073741824\n")
	write(t, filepath.Join(dev, "mem_info_vram_total"), "17179869184\n")
	write(t, filepath.Join(dev, "product_name"), "Instinct MI210\n")
	write(t, filepath.Join(dev, "hwmon", "hwmon3", "temp1_input"), "64000\n")
	write(t, filepath.Join(dev, "hwmon", "hwmon3", "power1_average"), "123400000\n")
	// A connector and an Intel card are ignored.
	write(t, filepath.Join(root, "class", "drm", "card1-HDMI-A-1", "status"), "connected\n")
	write(t, filepath.Join(root, "class", "drm", "card0", "device", "vendor"), "0x8086\n")

	r := &gpuReader{run: fakeRun(nil), sysfs: root, lookPath: func(string) (string, error) { return "", errors.New("no") }}
	g, probs := r.read(context.Background(), time.Now())
	if len(probs) != 0 || len(g) != 1 {
		t.Fatalf("gpus %+v problems %+v", g, probs)
	}
	if g[0].Vendor != "amd" || g[0].Name != "Instinct MI210" || g[0].UtilPercent != 42 || g[0].MemUsedMi != 1024 || g[0].MemTotalMi != 16384 ||
		g[0].TempC != 64 || g[0].PowerW != 123.4 {
		t.Fatalf("amd gpu: %+v", g[0])
	}
}

func TestGPUReaderNvidiaAndProblems(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "bus", "pci", "devices", "0000:00:1e.0", "vendor"), "0x10de\n")
	write(t, filepath.Join(root, "bus", "pci", "devices", "0000:00:1e.0", "class"), "0x030200\n")
	smi := "0, GPU-1, NVIDIA A10G, 99, 22000, 23028, 93, 290, 300\n"
	var calls []string
	r := &gpuReader{sysfs: root, lookPath: func(p string) (string, error) { return "/usr/bin/nvidia-smi", nil },
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, name+" "+strings.Join(args, " "))
			return []byte(smi), nil
		}}
	now := time.Now()
	g, probs := r.read(context.Background(), now)
	if len(g) != 1 || len(probs) != 0 || !strings.Contains(calls[0], "--query-gpu=") {
		t.Fatalf("gpus %+v problems %+v calls %v", g, probs, calls)
	}
	if p := gpuProblems(g); len(p) != 1 || p[0].Type != "GPUHot" || p[0].Severity != "high" {
		t.Fatalf("hot gpu: %+v", p)
	}

	// Driver broken: nvidia-smi fails while the card is on the bus.
	r.run = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver.\nMake sure..."), &exec.ExitError{}
	}
	if _, probs := r.read(context.Background(), now); len(probs) != 1 || probs[0].Severity != "high" || !strings.Contains(probs[0].Message, "couldn't communicate") {
		t.Fatalf("driver failure: %+v", probs)
	}

	// Card present, tools missing (noticed on the next hourly check).
	r.lookPath = func(string) (string, error) { return "", errors.New("no") }
	if _, probs := r.read(context.Background(), now.Add(gpuRecheck)); len(probs) != 1 || probs[0].Severity != "medium" {
		t.Fatalf("missing nvidia-smi: %+v", probs)
	}

	// nvidia-smi installed but no NVIDIA hardware: nothing to report.
	r2 := &gpuReader{sysfs: t.TempDir(), lookPath: func(string) (string, error) { return "/usr/bin/nvidia-smi", nil },
		run: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("No devices were found"), &exec.ExitError{}
		}}
	if g, probs := r2.read(context.Background(), now); len(g) != 0 || len(probs) != 0 {
		t.Fatalf("no hardware: %+v %+v", g, probs)
	}
}

func TestJournalFallsBackOnOldSystemd(t *testing.T) {
	a := &Agent{sys: fakeSys(t), secrets: newSecretScanner(),
		gpus: &gpuReader{sysfs: t.TempDir(), lookPath: func(string) (string, error) { return "", errors.New("no") }}}
	var withFields int
	a.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "journalctl" {
			return nil, errors.New("absent")
		}
		for _, x := range args {
			if strings.HasPrefix(x, "--output-fields") {
				withFields++
				return nil, errors.New("journalctl: unrecognized option '--output-fields'")
			}
		}
		return []byte(fmt.Sprintf(`{"__CURSOR":"c9","__REALTIME_TIMESTAMP":"%d","PRIORITY":"2","_SYSTEMD_UNIT":["db.service","db.service"],"MESSAGE":"disk failure"}`+"\n", time.Now().UnixMicro())), nil
	}
	snap := a.collect(context.Background(), time.Now())
	if a.cursor != "c9" || !a.oldJournal || len(snap.Host.ErrorLogs) != 1 || snap.Host.ErrorLogs[0].Unit != "db.service" {
		t.Fatalf("cursor %q old %v logs %+v", a.cursor, a.oldJournal, snap.Host.ErrorLogs)
	}
	a.collect(context.Background(), time.Now())
	if withFields != 1 {
		t.Fatalf("retried --output-fields %d times", withFields)
	}
}

// fakeStart hands out pipes standing in for long-running nvidia-smi processes.
type fakeStart struct {
	mu     sync.Mutex
	starts int
	args   []string
	w      *io.PipeWriter
}

func (f *fakeStart) start(_ context.Context, _ string, args ...string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	f.args = args
	r, w := io.Pipe()
	f.w = w
	return r, nil
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

func TestNvidiaStreamKeepsOneProcess(t *testing.T) {
	fs := &fakeStart{}
	oneShots := 0
	r := &gpuReader{sysfs: t.TempDir(), start: fs.start, lookPath: func(string) (string, error) { return "/usr/bin/nvidia-smi", nil },
		run: func(context.Context, string, ...string) ([]byte, error) {
			oneShots++
			return []byte("0, GPU-1, NVIDIA L4, 5, 100, 23034, 40, 20, 72\n"), nil
		}}
	now := time.Now()
	// First sample: the stream has just started, so nvidia-smi runs once.
	if g, _ := r.read(context.Background(), now); len(g) != 1 || g[0].UtilPercent != 5 || oneShots != 1 || fs.starts != 1 {
		t.Fatalf("first read: %+v oneShots=%d starts=%d", g, oneShots, fs.starts)
	}
	if strings.Join(fs.args, " ") != strings.Join(nvidiaQuery, " ")+" -lms 1000" {
		t.Fatalf("stream args: %v", fs.args)
	}
	fs.w.Write([]byte("0, GPU-1, NVIDIA L4, 77, 9000, 23034, 61, 55.5, 72\n"))
	waitFor(t, func() bool { r.stream.mu.Lock(); defer r.stream.mu.Unlock(); return !r.stream.last.IsZero() })
	g, probs := r.read(context.Background(), time.Now())
	if len(g) != 1 || g[0].UtilPercent != 77 || g[0].MemUsedMi != 9000 || len(probs) != 0 || oneShots != 1 || fs.starts != 1 {
		t.Fatalf("streamed read: %+v probs=%v oneShots=%d starts=%d", g, probs, oneShots, fs.starts)
	}
	// nvidia-smi exits: back to one-shot, restarted only after a pause.
	fs.w.Close()
	waitFor(t, func() bool { r.stream.mu.Lock(); defer r.stream.mu.Unlock(); return !r.stream.running })
	r.read(context.Background(), time.Now())
	if oneShots != 2 || fs.starts != 1 {
		t.Fatalf("after exit: oneShots=%d starts=%d", oneShots, fs.starts)
	}
	r.read(context.Background(), time.Now().Add(nvidiaRestart+time.Second))
	if fs.starts != 2 {
		t.Fatalf("not restarted after %v: starts=%d", nvidiaRestart, fs.starts)
	}
}

func TestNvidiaStreamSilentFallsBack(t *testing.T) {
	fs := &fakeStart{}
	oneShots := 0
	r := &gpuReader{sysfs: t.TempDir(), start: fs.start, lookPath: func(string) (string, error) { return "/usr/bin/nvidia-smi", nil },
		run: func(context.Context, string, ...string) ([]byte, error) {
			oneShots++
			return []byte("0, GPU-1, NVIDIA L4, 5, 100, 23034, 40, 20, 72\n"), nil
		}}
	now := time.Now()
	r.read(context.Background(), now)
	// Nothing printed for 3 intervals: stop using the stream for good.
	if g, _ := r.read(context.Background(), now.Add(nvidiaStale+time.Second)); len(g) != 1 {
		t.Fatalf("no fallback data: %+v", g)
	}
	r.read(context.Background(), now.Add(time.Hour))
	if !r.stream.unusable || fs.starts != 1 || oneShots != 3 {
		t.Fatalf("unusable=%v starts=%d oneShots=%d", r.stream.unusable, fs.starts, oneShots)
	}
}

func TestNvidiaSMIFailureReportedWithoutPCIMatch(t *testing.T) {
	r := &gpuReader{sysfs: t.TempDir(), lookPath: func(string) (string, error) { return "/usr/bin/nvidia-smi", nil },
		run: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("Failed to initialize NVML: Insufficient Permissions"), &exec.ExitError{}
		}}
	if _, probs := r.read(context.Background(), time.Now()); len(probs) != 1 || !strings.Contains(probs[0].Message, "Insufficient Permissions") {
		t.Fatalf("problems: %+v", probs)
	}
}

// TestExecStartWithFakeNvidiaSMI runs a stand-in nvidia-smi that prints a
// sample every 50ms, like "-l", and checks lines arrive and it stops with ctx.
func TestExecStartWithFakeNvidiaSMI(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	smi := filepath.Join(dir, "nvidia-smi")
	// Like nvidia-smi: one sample, or one every interval with "-l".
	write(t, smi, "#!/bin/sh\nsample() { echo '0, GPU-1, NVIDIA L4, 42, 1000, 23034, 50, 30, 72'; echo '1, GPU-2, NVIDIA L4, 7, 10, 23034, 35, 16, 72'; }\n"+
		"case \"$*\" in *-l*) while true; do sample; sleep 0.05; done ;; *) sample ;; esac\n")
	if err := os.Chmod(smi, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &gpuReader{sysfs: dir, start: execStart, run: execRun, lookPath: func(string) (string, error) { return smi, nil }}
	r.read(ctx, time.Now())
	waitFor(t, func() bool { r.stream.mu.Lock(); defer r.stream.mu.Unlock(); return len(r.stream.seen) == 2 })
	g, _ := r.read(ctx, time.Now())
	if len(g) != 2 || g[0].UtilPercent != 42 || g[1].Index != 1 {
		t.Fatalf("streamed: %+v", g)
	}
	cancel() // agent shutting down
	waitFor(t, func() bool { r.stream.mu.Lock(); defer r.stream.mu.Unlock(); return !r.stream.running })
}

// A snapshot reports the average and peak of every stream sample in its
// window, so a burst between snapshots is not lost.
func TestNvidiaStreamAveragesWindow(t *testing.T) {
	fs := &fakeStart{}
	r := &gpuReader{sysfs: t.TempDir(), start: fs.start, lookPath: func(string) (string, error) { return "/usr/bin/nvidia-smi", nil },
		run: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("0, GPU-1, NVIDIA A40, 0, 100, 46068, 30, 20, 300\n"), nil
		}}
	r.read(context.Background(), time.Now())
	for _, u := range []string{"0", "90", "30"} {
		fs.w.Write([]byte("0, GPU-1, NVIDIA A40, " + u + ", 4000, 46068, 45, 120, 300\n"))
	}
	waitFor(t, func() bool { r.stream.mu.Lock(); defer r.stream.mu.Unlock(); return r.stream.utilN[0] == 3 })
	g, _ := r.read(context.Background(), time.Now())
	if len(g) != 1 || g[0].UtilPercent != 40 || g[0].UtilPeakPercent != 90 || g[0].MemUsedMi != 4000 {
		t.Fatalf("want avg 40 peak 90: %+v", g)
	}
	// The window resets: one new sample reads as itself, with no peak.
	fs.w.Write([]byte("0, GPU-1, NVIDIA A40, 12, 4000, 46068, 45, 120, 300\n"))
	waitFor(t, func() bool { r.stream.mu.Lock(); defer r.stream.mu.Unlock(); return r.stream.utilN[0] == 1 })
	if g, _ = r.read(context.Background(), time.Now()); g[0].UtilPercent != 12 || g[0].UtilPeakPercent != 0 {
		t.Fatalf("after reset: %+v", g)
	}
}
