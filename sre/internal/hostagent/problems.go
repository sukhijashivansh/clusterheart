package hostagent

import (
	"fmt"

	"clusterheart/sre/internal/contract"
)

// Thresholds match the dashboard's green / yellow / red rules.
const (
	diskWarnPct, diskBadPct = 80.0, 90.0
	memWarnPct, memBadPct   = 80.0, 90.0
	loadWarn, loadBad       = 1.0, 2.0 // 5-minute load per core
	swapWarnPct             = 50.0
	recentRebootSec         = 15 * 60
)

func pct(used, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

// problems derives measured issues from a host snapshot.
func problems(h contract.HostSnapshot) []contract.HostProblem {
	var out []contract.HostProblem
	add := func(typ, sev, msg string) {
		out = append(out, contract.HostProblem{Type: typ, Severity: sev, Message: msg})
	}
	for _, d := range h.Disks {
		switch p := pct(d.UsedMi, d.TotalMi); {
		case p >= diskBadPct:
			add("DiskFull", "high", fmt.Sprintf("%s is %.0f%% full (%d of %d MiB)", d.Mount, p, d.UsedMi, d.TotalMi))
		case p >= diskWarnPct:
			add("DiskFull", "medium", fmt.Sprintf("%s is %.0f%% full", d.Mount, p))
		}
	}
	switch p := pct(h.MemUsedMi, h.MemTotalMi); {
	case p >= memBadPct:
		add("MemoryPressure", "high", fmt.Sprintf("memory %.0f%% used (%d of %d MiB)", p, h.MemUsedMi, h.MemTotalMi))
	case p >= memWarnPct:
		add("MemoryPressure", "medium", fmt.Sprintf("memory %.0f%% used", p))
	}
	if h.CPUCores > 0 {
		switch per := h.Load5 / float64(h.CPUCores); {
		case per >= loadBad:
			add("HighLoad", "high", fmt.Sprintf("5-minute load %.2f on %d cores", h.Load5, h.CPUCores))
		case per >= loadWarn:
			add("HighLoad", "medium", fmt.Sprintf("5-minute load %.2f on %d cores", h.Load5, h.CPUCores))
		}
	}
	if p := pct(h.SwapUsedMi, h.SwapTotalMi); h.SwapTotalMi > 0 && p >= swapWarnPct {
		add("SwapPressure", "medium", fmt.Sprintf("swap %.0f%% used", p))
	}
	for _, u := range h.FailedUnits {
		add("FailedService", "high", u.Name+" has failed")
	}
	if h.UptimeSec > 0 && h.UptimeSec < recentRebootSec {
		add("RecentReboot", "medium", fmt.Sprintf("rebooted %d minutes ago", h.UptimeSec/60))
	}
	return append(out, gpuProblems(h.GPUs)...)
}
