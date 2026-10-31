package hostagent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
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
