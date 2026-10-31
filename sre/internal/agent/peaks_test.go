package agent

import (
	"testing"
	"time"
)

func TestPeakTrackerRollingMax(t *testing.T) {
	tr := newPeakTracker()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if c, m, w := tr.observe("ns/p", 100, 50, t0); c != 100 || m != 50 || w != 0 {
		t.Fatalf("first sample: got %d %d %d", c, m, w)
	}
	// A spike, then lower readings: the peak holds.
	tr.observe("ns/p", 400, 80, t0.Add(30*time.Minute))
	if c, m, w := tr.observe("ns/p", 120, 60, t0.Add(2*time.Hour)); c != 400 || m != 80 || w != 120 {
		t.Fatalf("after spike: got %d %d %d", c, m, w)
	}
	// Once the spike's hour leaves the 6h window, the peak drops and the window
	// is capped at 6h.
	c, m, w := tr.observe("ns/p", 110, 55, t0.Add(7*time.Hour))
	if c != 120 || m != 60 {
		t.Fatalf("after window: got peak %d %d", c, m)
	}
	if w > int(peakWindow.Minutes()) {
		t.Fatalf("window %d exceeds %v", w, peakWindow)
	}
}

func TestPeakTrackerPrune(t *testing.T) {
	tr := newPeakTracker()
	t0 := time.Now()
	tr.observe("ns/gone", 1, 1, t0)
	tr.observe("ns/live", 1, 1, t0.Add(2*time.Hour))
	tr.prune(t0.Add(2 * time.Hour))
	if _, ok := tr.pods["ns/gone"]; ok {
		t.Fatal("stale pod not pruned")
	}
	if _, ok := tr.pods["ns/live"]; !ok {
		t.Fatal("live pod pruned")
	}
}
