package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHostSnapshotText(t *testing.T) {
	raw := json.RawMessage(`{"collectedAt":"2026-10-31T20:00:00Z","host":{"hostname":"web-01","os":"Ubuntu 24.04 LTS","cpuCores":4,"cpuPercent":12.5,"load5":1.5,"memTotalMi":8000,"memUsedMi":7200,"uptimeSec":7200,"netRxKBps":120.5,"netTxKBps":30,
"gpus":[{"index":0,"vendor":"nvidia","name":"NVIDIA A10G","utilPercent":87,"memUsedMi":20000,"memTotalMi":23028,"tempC":71,"powerW":180}],
"disks":[{"mount":"/","fs":"ext4","totalMi":50000,"usedMi":46000}],"processes":[{"pid":42,"name":"java","user":"app","cpuPercent":80.5,"memMi":2048}],
"failedUnits":[{"name":"nginx.service"}],"errorLogs":[{"time":"2026-10-31T19:59:00Z","unit":"api.service","message":"db timeout"}],"problems":[{"type":"DiskFull","severity":"high","message":"/ is 92% full"}]},
"secretFindings":[{"source":"log","type":"jwt","owner":"Service/api","container":"api.service","count":2}]}`)
	var b strings.Builder
	writeSnapshotText(&b, raw)
	out := b.String()
	for _, want := range []string{"Host web-01", "CPU 12.5%", "Memory 7200 of 8000", "/ ext4 46000/50000", "java 42 app 80.5 2048", "Network in 120.5 KB/s, out 30.0 KB/s", "0 NVIDIA A10G 87% 20000/23028 71 180", "cpu% memMiB", "nginx.service", "[high] DiskFull", "api.service: db timeout", "jwt in log of Service/api"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestServerTrendText(t *testing.T) {
	var h serverHistory
	if err := json.Unmarshal([]byte(`{"points":[
{"t":"2026-10-30T02:00:00Z","cpu":20,"cpuMax":95,"mem":50,"memMax":60,"disk":70,"load":1,"loadMax":4},
{"t":"2026-10-31T02:00:00Z","cpu":10,"cpuMax":30,"mem":70,"memMax":90,"disk":72,"load":0.5,"loadMax":1}],
"gpus":[{"index":0,"name":"NVIDIA A10G","points":[{"util":50,"utilMax":100,"temp":60,"tempMax":80},{"util":30,"utilMax":40,"temp":50,"tempMax":55}]}]}`), &h); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	writeTrendText(&b, h)
	out := b.String()
	for _, want := range []string{"CPU 15% / 95% at 2026-10-30T02:00:00Z", "Memory 60% / 90% at 2026-10-31T02:00:00Z", "(now 72%)", "Load 0.75 / 4.00", "GPU 0 NVIDIA A10G: utilization 40% / 100%, temperature 55 / 80°C"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
