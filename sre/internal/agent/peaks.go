package agent

import (
	"sync"
	"time"
)

// peakWindow is how far back the rolling per-pod peak reaches. Rightsizing
// waits for most of a full window before it suggests a change.
const peakWindow = 6 * time.Hour

// peakBucket is the max usage seen during one hour.
type peakBucket struct {
	hour     int64 // unix hour
	cpu, mem int64
}

type podPeaks struct {
	first    time.Time // first sample in the current window
	lastSeen time.Time
	buckets  []peakBucket // oldest first, at most peakWindow/1h+1
}

// peakTracker keeps a rolling max of CPU/memory per pod in hourly buckets, so
// the snapshot can carry "peak over the last N hours" without storing every
// sample. It lives in agent memory and resets when the agent restarts.
type peakTracker struct {
	mu   sync.Mutex
	pods map[string]*podPeaks
}

func newPeakTracker() *peakTracker {
	return &peakTracker{pods: map[string]*podPeaks{}}
}

// observe records one sample and returns the peak CPU (millicores), peak
// memory (MiB) and how many minutes of history the peak covers.
func (t *peakTracker) observe(key string, cpu, mem int64, now time.Time) (int64, int64, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	hour := now.Unix() / 3600
	oldest := now.Add(-peakWindow).Unix() / 3600
	pp := t.pods[key]
	if pp == nil {
		pp = &podPeaks{first: now}
		t.pods[key] = pp
	}
	pp.lastSeen = now
	kept := pp.buckets[:0]
	for _, b := range pp.buckets {
		if b.hour >= oldest {
			kept = append(kept, b)
		}
	}
	pp.buckets = kept
	if n := len(pp.buckets); n > 0 && pp.buckets[n-1].hour == hour {
		b := &pp.buckets[n-1]
		b.cpu, b.mem = max(b.cpu, cpu), max(b.mem, mem)
	} else {
		pp.buckets = append(pp.buckets, peakBucket{hour: hour, cpu: cpu, mem: mem})
	}
	if start := time.Unix(pp.buckets[0].hour*3600, 0); pp.first.Before(start) {
		pp.first = start
	}
	var pc, pm int64
	for _, b := range pp.buckets {
		pc, pm = max(pc, b.cpu), max(pm, b.mem)
	}
	return pc, pm, int(now.Sub(pp.first).Minutes())
}

// prune forgets pods that haven't been seen for an hour (deleted or rescheduled
// under a new name), so the map can't grow without bound.
func (t *peakTracker) prune(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, pp := range t.pods {
		if now.Sub(pp.lastSeen) > time.Hour {
			delete(t.pods, k)
		}
	}
}
