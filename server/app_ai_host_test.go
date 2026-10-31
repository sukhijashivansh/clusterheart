package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHostSnapshotText(t *testing.T) {
	raw := json.RawMessage(`{"collectedAt":"2026-10-31T20:00:00Z","host":{"hostname":"web-01","os":"Ubuntu 24.04 LTS","cpuCores":4,"cpuPercent":12.5,"load5":1.5,"memTotalMi":8000,"memUsedMi":7200,"uptimeSec":7200,
"disks":[{"mount":"/","fs":"ext4","totalMi":50000,"usedMi":46000}],"processes":[{"pid":42,"name":"java","user":"app","cpuPercent":80.5,"memMi":2048}],
"failedUnits":[{"name":"nginx.service"}],"errorLogs":[{"time":"2026-10-31T19:59:00Z","unit":"api.service","message":"db timeout"}],"problems":[{"type":"DiskFull","severity":"high","message":"/ is 92% full"}]},
"secretFindings":[{"source":"log","type":"jwt","owner":"Service/api","container":"api.service","count":2}]}`)
	var b strings.Builder
	writeSnapshotText(&b, raw)
	out := b.String()
	for _, want := range []string{"Host web-01", "CPU 12.5%", "Memory 7200 of 8000", "/ ext4 46000/50000", "java 42 app 80.5 2048", "nginx.service", "[high] DiskFull", "api.service: db timeout", "jwt in log of Service/api"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
