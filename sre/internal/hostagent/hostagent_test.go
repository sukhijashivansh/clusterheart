package hostagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"clusterheart/sre/internal/contract"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func init() { primeWait = 10 * time.Millisecond }

// fakeSys builds a minimal /proc and /etc.
func fakeSys(t *testing.T) *sys {
	root := t.TempDir()
	proc, etc := filepath.Join(root, "proc"), filepath.Join(root, "etc")
	write(t, filepath.Join(proc, "stat"), "cpu  100 0 100 800 0 0 0 0 0 0\ncpu0 50 0 50 400 0 0 0 0 0 0\ncpu1 50 0 50 400 0 0 0 0 0 0\n")
	write(t, filepath.Join(proc, "meminfo"), "MemTotal:       8192000 kB\nMemFree:  1000000 kB\nMemAvailable:   2048000 kB\nSwapTotal: 1024000 kB\nSwapFree: 512000 kB\n")
	write(t, filepath.Join(proc, "loadavg"), "0.50 4.20 3.00 2/300 1234\n")
	write(t, filepath.Join(proc, "uptime"), "600.5 1000.0\n")
	write(t, filepath.Join(proc, "sys", "kernel", "osrelease"), "6.8.0-45-generic\n")
	write(t, filepath.Join(proc, "sys", "kernel", "hostname"), "web-01\n")
	write(t, filepath.Join(proc, "mounts"), "/dev/sda1 / ext4 rw 0 0\ntmpfs /run tmpfs rw 0 0\n/dev/sdb1 /data xfs rw 0 0\n/dev/sda1 /var/lib/docker ext4 rw 0 0\n")
	// 200 MiB resident, in pages (field 24).
	write(t, filepath.Join(proc, "42", "stat"), fmt.Sprintf("42 (my app) S 1 42 42 0 -1 4194560 100 0 0 0 300 100 0 0 20 0 1 0 100 1000 %d 18446744073709551615\n", (200<<20)/pageSize))
	write(t, filepath.Join(proc, "2", "stat"), "2 (kthreadd) S 0 0 0 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 1 0 0 18446744073709551615\n")
	write(t, filepath.Join(proc, "net", "dev"), netDev(1<<20, 1<<19))
	write(t, filepath.Join(etc, "os-release"), "NAME=\"Ubuntu\"\nPRETTY_NAME=\"Ubuntu 24.04 LTS\"\n")
	write(t, filepath.Join(etc, "passwd"), "root:x:0:0::/root:/bin/bash\napp:x:1000:1000::/home/app:/bin/bash\n")
	return &sys{proc: proc, etc: etc, sysfs: filepath.Join(root, "sys"), uidOf: func(dir string) int {
		if filepath.Base(dir) == "42" {
			return 1000
		}
		return 0
	}, statfs: func(p string) (int64, int64, error) {
		if p == "/" {
			return 50000, 46000, nil // 92%
		}
		return 100000, 10000, nil
	}}
}

// netDev renders /proc/net/dev with eth0 at the given byte counters, plus
// loopback and container traffic that must not be counted.
func netDev(rx, tx uint64) string {
	return "Inter-|   Receive                                                |  Transmit\n" +
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n" +
		"    lo: 999999 10 0 0 0 0 0 0 999999 10 0 0 0 0 0 0\n" +
		fmt.Sprintf("  eth0: %d 100 0 0 0 0 0 0 %d 50 0 0 0 0 0 0\n", rx, tx) +
		"veth1a2b: 888888 5 0 0 0 0 0 0 888888 5 0 0 0 0 0 0\n"
}

func TestSampleReadsProcAndEtc(t *testing.T) {
	s := fakeSys(t)
	h := s.sample(time.Now())
	if h.Hostname != "web-01" || h.OS != "Ubuntu 24.04 LTS" || h.Kernel != "6.8.0-45-generic" || h.CPUCores != 2 {
		t.Fatalf("identity: %+v", h)
	}
	if h.MemTotalMi != 8000 || h.MemUsedMi != 6000 || h.SwapTotalMi != 1000 || h.SwapUsedMi != 500 {
		t.Fatalf("memory: %+v", h)
	}
	if h.Load5 != 4.2 || h.UptimeSec != 600 {
		t.Fatalf("load/uptime: %+v", h)
	}
	if len(h.Disks) != 2 || h.Disks[0].Mount != "/" || h.Disks[1].Mount != "/data" {
		t.Fatalf("disks (tmpfs and duplicate devices excluded): %+v", h.Disks)
	}
	if len(h.Processes) != 1 || h.Processes[0].Name != "my app" || h.Processes[0].User != "app" || h.Processes[0].MemMi != 200 {
		t.Fatalf("processes (kernel threads excluded): %+v", h.Processes)
	}
	// CPU: 200 busy ticks out of 1000 since the fake counters started at zero.
	write(t, filepath.Join(s.proc, "stat"), "cpu  200 0 200 1600 0 0 0 0 0 0\ncpu0 1 0 1 1 0 0 0 0\ncpu1 1 0 1 1 0 0 0 0\n")
	// eth0 receives 100 KiB/s and sends 50 KiB/s over the next ~30s.
	elapsed := 30*time.Second - primeWait
	write(t, filepath.Join(s.proc, "net", "dev"), netDev(1<<20+uint64(elapsed.Seconds()*100*1024), 1<<19+uint64(elapsed.Seconds()*50*1024)))
	h = s.sample(time.Now().Add(30 * time.Second))
	if h.CPUPercent != 20 {
		t.Fatalf("cpu%% = %v", h.CPUPercent)
	}
	if h.NetRxKBps < 99 || h.NetRxKBps > 101 || h.NetTxKBps < 49 || h.NetTxKBps > 51 {
		t.Fatalf("network rates (loopback and veth excluded): rx %v tx %v", h.NetRxKBps, h.NetTxKBps)
	}
}

func TestProblemsThresholds(t *testing.T) {
	s := fakeSys(t)
	h := s.sample(time.Now())
	h.FailedUnits = []contract.HostUnit{{Name: "nginx.service"}}
	h.MemUsedMi = 6800 // 85% of 8000: above the 80% warning, below the 90% alarm
	got := map[string]string{}
	for _, p := range problems(h) {
		got[p.Type] = p.Severity
	}
	want := map[string]string{"DiskFull": "high", "MemoryPressure": "medium", "HighLoad": "high", "SwapPressure": "medium", "FailedService": "high", "RecentReboot": "medium"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
}

func fakeRun(outputs map[string]string) runFunc {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		return []byte(outputs[name+" "+args[0]]), nil
	}
}

func TestJournalUnitsAndEnvParsing(t *testing.T) {
	run := fakeRun(map[string]string{
		"systemctl list-units": "● nginx.service loaded failed failed A high performance web server\nbackup.service loaded failed failed Nightly backup\n",
		"systemctl show":       "Id=api.service\nEnvironment=PORT=8080 \"DB_PASSWORD=hunter22 hunter\" LOG_LEVEL=info\n\nId=cron.service\nEnvironment=\n",
		"journalctl -o": `{"__CURSOR":"c1","__REALTIME_TIMESTAMP":"1761937200000000","PRIORITY":"3","_SYSTEMD_UNIT":"api.service","MESSAGE":"db error: connect refused"}
{"__CURSOR":"c2","__REALTIME_TIMESTAMP":"1761937201000000","PRIORITY":"6","SYSLOG_IDENTIFIER":"worker","MESSAGE":[116,111,107,101,110,61,97,98,99,100,101,102,103,104]}
`,
	})
	units := failedUnits(context.Background(), run)
	if len(units) != 2 || units[0].Name != "nginx.service" || units[0].Description != "A high performance web server" {
		t.Fatalf("failed units: %+v", units)
	}
	env := unitEnv(context.Background(), run)
	if got := env["api.service"]; len(got) != 3 || got[1] != "DB_PASSWORD=hunter22 hunter" {
		t.Fatalf("unit env: %+v", env)
	}
	entries, cursor, err := readJournal(context.Background(), run, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != "c2" || len(entries) != 2 || entries[0].priority != 3 || entries[0].unit != "api.service" {
		t.Fatalf("journal: %q %+v", cursor, entries)
	}
	if entries[1].message != "token=abcdefgh" || entries[1].unit != "worker" {
		t.Fatalf("byte-array message: %+v", entries[1])
	}
}

func TestSecretsFromJournalAndUnitEnv(t *testing.T) {
	s := newSecretScanner()
	s.host = "web-01"
	now := time.Now()
	s.recordJournal([]journalEntry{{at: now, unit: "api.service", message: `{"msg":"fetched","secret":"s3cr3tvalue"}`}}, now)
	s.scanUnitEnv(map[string][]string{"api.service": {"DB_PASSWORD=hunter22hunter", "PORT=8080"}}, now)
	got := s.findings(now)
	if len(got) != 2 {
		t.Fatalf("findings: %+v", got)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "s3cr3tvalue") || strings.Contains(string(raw), "hunter22hunter") {
		t.Fatalf("findings carry raw values: %s", raw)
	}
	for _, f := range got {
		if f.Owner != "Service/api" || f.Namespace != "web-01" {
			t.Errorf("owner/host: %+v", f)
		}
		if f.Masked == "" || f.Length == 0 {
			t.Errorf("no masked hint: %+v", f)
		}
	}
	if dump, _ := json.Marshal([]any{s.logs, s.specs}); strings.Contains(string(dump), "s3cr3tvalue") || strings.Contains(string(dump), "hunter22hunter") {
		t.Fatalf("scanner keeps raw values: %s", dump)
	}
	s.scanUnitEnv(map[string][]string{}, now) // the setting was removed
	if len(s.findings(now)) != 1 {
		t.Fatal("env finding not cleared after the fix")
	}
}

func TestAgentCycleSendsHostSnapshot(t *testing.T) {
	var mu sync.Mutex
	var batch contract.TelemetryBatch
	polls := 0
	heartbeats := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("X-Cluster-ID") != "srv_1" {
			w.WriteHeader(401)
			return
		}
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/api/v1/telemetry":
			_ = json.Unmarshal(body, &batch)
		case "/api/v1/agents/heartbeat":
			heartbeats++
		default:
			polls++
		}
	}))
	defer ts.Close()

	a := New(NewClient(ts.URL, "srv_1", "tok"), time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.sys = fakeSys(t)
	a.run = fakeRun(map[string]string{"systemctl show": "Id=api.service\nEnvironment=DB_PASSWORD=hunter22hunter\n"})
	a.gpus = &gpuReader{run: a.run, sysfs: t.TempDir(), lookPath: func(string) (string, error) { return "", errors.New("not found") }}
	a.cycle(context.Background())

	mu.Lock()
	if len(batch.Snapshots) != 1 || batch.Snapshots[0].Host == nil || batch.Snapshots[0].Host.Hostname != "web-01" || heartbeats != 1 {
		mu.Unlock()
		t.Fatalf("telemetry: %+v heartbeats=%d", batch, heartbeats)
	}
	if len(batch.Snapshots[0].SecretFindings) != 1 {
		mu.Unlock()
		t.Fatalf("findings not sent: %+v", batch.Snapshots[0].SecretFindings)
	}
	if polls != 0 {
		mu.Unlock()
		t.Fatalf("agent called %d other endpoints (reveals were removed)", polls)
	}
	if strings.Contains(fmt.Sprint(batch), "hunter22hunter") {
		mu.Unlock()
		t.Fatal("raw secret sent")
	}
	mu.Unlock()
}

// TestRealHost samples the machine running the tests (Linux only).
func TestRealHost(t *testing.T) {
	if _, err := os.Stat("/proc/stat"); err != nil {
		t.Skip("no /proc")
	}
	s := &sys{proc: "/proc", etc: "/etc", sysfs: "/sys", statfs: statfsMi, uidOf: fileOwner}
	h := s.sample(time.Now())
	if h.CPUCores < 1 || h.MemTotalMi <= 0 || h.Hostname == "" || h.UptimeSec <= 0 {
		t.Fatalf("real host sample looks wrong: %+v", h)
	}
	if h.CPUPercent < 0 || h.CPUPercent > 100 {
		t.Fatalf("cpu%% out of range: %v", h.CPUPercent)
	}
	t.Logf("real host: %s, %d cores, cpu %.1f%%, mem %d/%d MiB, %d disks, %d procs", h.OS, h.CPUCores, h.CPUPercent, h.MemUsedMi, h.MemTotalMi, len(h.Disks), len(h.Processes))
}
