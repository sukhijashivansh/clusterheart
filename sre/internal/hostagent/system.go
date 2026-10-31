// Package hostagent is the Clusterheart agent for a plain Linux server. It
// reads the machine's own state from /proc, statfs, systemd and the journal,
// as an unprivileged user, and sends bounded snapshots to the control plane
// over outbound HTTPS. It has no code path that changes the machine.
package hostagent

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"clusterheart/sre/internal/contract"
)

// sys reads system state under configurable roots so tests can point it at a
// fake /proc and /etc.
type sys struct {
	proc, etc, sysfs string
	statfs           func(path string) (totalMi, usedMi int64, err error)
	// uidOf returns the owner of /proc/<pid> (the process's user). Tests fake it.
	uidOf func(path string) int

	prevCPU      cpuTimes
	prevProcs    map[int]uint64 // pid -> utime+stime ticks
	prevNet      netBytes
	prevSampleAt time.Time
	users        map[int]string
}

type netBytes struct{ rx, tx uint64 }

// primeWait is how long the first sample waits so CPU usage is measured over
// a window long enough for the 10ms tick resolution (a 0.5s window turned a
// few ticks of the agent's own startup into "6% CPU").
var primeWait = 5 * time.Second

type cpuTimes struct{ total, idle uint64 }

const clockTicks = 100 // USER_HZ on Linux

// maxProcesses bounds the process list in a snapshot.
const maxProcesses = 15

// realFS are filesystem types worth reporting (no tmpfs, overlay, proc, ...).
var realFS = map[string]bool{"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true, "f2fs": true, "vfat": true, "jfs": true, "reiserfs": true, "nfs": true, "nfs4": true}

func readLines(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func readFirst(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (s *sys) cpuTimes() cpuTimes {
	for _, l := range readLines(filepath.Join(s.proc, "stat")) {
		f := strings.Fields(l)
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		var t cpuTimes
		for i, v := range f[1:] {
			n, _ := strconv.ParseUint(v, 10, 64)
			if i < 8 { // user nice system idle iowait irq softirq steal (guest is already in user)
				t.total += n
			}
			if i == 3 || i == 4 { // idle + iowait
				t.idle += n
			}
		}
		return t
	}
	return cpuTimes{}
}

func (s *sys) cpuCores() int {
	n := 0
	for _, l := range readLines(filepath.Join(s.proc, "stat")) {
		if strings.HasPrefix(l, "cpu") && len(l) > 3 && l[3] >= '0' && l[3] <= '9' {
			n++
		}
	}
	if n == 0 {
		n = 1
	}
	return n
}

// memInfo returns MiB values from /proc/meminfo.
func (s *sys) memInfo() (total, used, swapTotal, swapUsed int64) {
	kv := map[string]int64{}
	for _, l := range readLines(filepath.Join(s.proc, "meminfo")) {
		f := strings.Fields(l)
		if len(f) >= 2 {
			n, _ := strconv.ParseInt(f[1], 10, 64)
			kv[strings.TrimSuffix(f[0], ":")] = n // kB
		}
	}
	avail, ok := kv["MemAvailable"]
	if !ok {
		avail = kv["MemFree"] + kv["Buffers"] + kv["Cached"]
	}
	total = kv["MemTotal"] / 1024
	used = (kv["MemTotal"] - avail) / 1024
	swapTotal = kv["SwapTotal"] / 1024
	swapUsed = (kv["SwapTotal"] - kv["SwapFree"]) / 1024
	return
}

func (s *sys) loadAvg() (l1, l5, l15 float64) {
	f := strings.Fields(readFirst(filepath.Join(s.proc, "loadavg")))
	if len(f) >= 3 {
		l1, _ = strconv.ParseFloat(f[0], 64)
		l5, _ = strconv.ParseFloat(f[1], 64)
		l15, _ = strconv.ParseFloat(f[2], 64)
	}
	return
}

func (s *sys) uptime() int64 {
	f := strings.Fields(readFirst(filepath.Join(s.proc, "uptime")))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return int64(v)
}

func (s *sys) osName() string {
	for _, l := range readLines(filepath.Join(s.etc, "os-release")) {
		if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

func (s *sys) kernel() string {
	return readFirst(filepath.Join(s.proc, "sys", "kernel", "osrelease"))
}

func (s *sys) hostname() string {
	if h := readFirst(filepath.Join(s.proc, "sys", "kernel", "hostname")); h != "" {
		return h
	}
	h, _ := os.Hostname()
	return h
}

// disks lists real mounted filesystems once per device.
func (s *sys) disks() []contract.HostDisk {
	seen := map[string]bool{}
	var out []contract.HostDisk
	for _, l := range readLines(filepath.Join(s.proc, "mounts")) {
		f := strings.Fields(l)
		if len(f) < 3 || !realFS[f[2]] || seen[f[0]] {
			continue
		}
		mount := strings.ReplaceAll(f[1], `\040`, " ")
		if s.statfs == nil {
			continue
		}
		total, used, err := s.statfs(mount)
		if err != nil || total == 0 {
			continue
		}
		seen[f[0]] = true
		out = append(out, contract.HostDisk{Mount: mount, FS: f[2], TotalMi: total, UsedMi: used})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mount < out[j].Mount })
	return out
}

func (s *sys) userName(uid int) string {
	if s.users == nil {
		s.users = map[int]string{}
		for _, l := range readLines(filepath.Join(s.etc, "passwd")) {
			f := strings.Split(l, ":")
			if len(f) >= 3 {
				if id, err := strconv.Atoi(f[2]); err == nil {
					s.users[id] = f[0]
				}
			}
		}
	}
	if n, ok := s.users[uid]; ok {
		return n
	}
	return strconv.Itoa(uid)
}

// processes returns the busiest processes: CPU% since the previous sample
// (percent of one core) and resident memory. It reads one small file per
// process (/proc/<pid>/stat); the user comes from the directory's owner.
func (s *sys) processes(now time.Time) []contract.HostProcess {
	entries, _ := os.ReadDir(s.proc)
	elapsed := now.Sub(s.prevSampleAt).Seconds()
	cur := make(map[int]uint64, len(s.prevProcs))
	var out []contract.HostProcess
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(s.proc, e.Name())
		stat := readFirst(filepath.Join(dir, "stat"))
		// comm is in parentheses and may contain spaces; fields follow ")".
		open, close := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
		if open < 0 || close < open {
			continue
		}
		name := stat[open+1 : close]
		rest := strings.Fields(stat[close+1:])
		if len(rest) < 22 {
			continue
		}
		// rest[0] is field 3 (state): utime=14, stime=15, rss=24 (pages).
		ut, _ := strconv.ParseUint(rest[11], 10, 64)
		st, _ := strconv.ParseUint(rest[12], 10, 64)
		rssPages, _ := strconv.ParseInt(rest[21], 10, 64)
		ticks := ut + st
		cur[pid] = ticks
		if rssPages <= 0 { // kernel threads
			continue
		}
		cpu := 0.0
		if prev, ok := s.prevProcs[pid]; ok && elapsed > 0 && ticks >= prev {
			cpu = float64(ticks-prev) / clockTicks / elapsed * 100
		}
		p := contract.HostProcess{PID: pid, Name: name, CPUPercent: round1(cpu), MemMi: rssPages * int64(pageSize) / (1 << 20)}
		if s.uidOf != nil {
			if uid := s.uidOf(dir); uid >= 0 {
				p.User = s.userName(uid)
			}
		}
		out = append(out, p)
	}
	s.prevProcs = cur
	sort.Slice(out, func(i, j int) bool {
		if out[i].CPUPercent != out[j].CPUPercent {
			return out[i].CPUPercent > out[j].CPUPercent
		}
		return out[i].MemMi > out[j].MemMi
	})
	if len(out) > maxProcesses {
		out = out[:maxProcesses]
	}
	return out
}

var pageSize = os.Getpagesize()

// virtualIface marks interfaces whose traffic is already counted on a
// physical one (containers, bridges, VPN tunnels).
func virtualIface(name string) bool {
	if name == "lo" {
		return true
	}
	for _, p := range []string{"veth", "docker", "br-", "cni", "flannel", "cali", "virbr", "vnet", "tun", "tap", "kube", "lxc", "vxlan", "wg"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// netTotals sums received and sent bytes over physical interfaces.
func (s *sys) netTotals() netBytes {
	var n netBytes
	for _, l := range readLines(filepath.Join(s.proc, "net", "dev")) {
		name, rest, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		f := strings.Fields(rest)
		if virtualIface(name) || len(f) < 9 {
			continue
		}
		rx, _ := strconv.ParseUint(f[0], 10, 64)
		tx, _ := strconv.ParseUint(f[8], 10, 64)
		n.rx += rx
		n.tx += tx
	}
	return n
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

// sample fills the machine metrics of a host snapshot. CPU usage is measured
// against the previous sample, so the first call primes it with a short wait.
func (s *sys) sample(now time.Time) contract.HostSnapshot {
	if s.prevSampleAt.IsZero() {
		s.prevCPU = s.cpuTimes()
		s.prevNet = s.netTotals()
		s.prevSampleAt = now
		s.processes(now) // prime per-process counters
		time.Sleep(primeWait)
		now = now.Add(primeWait)
	}
	elapsed := now.Sub(s.prevSampleAt).Seconds()
	cpu := s.cpuTimes()
	pct := 0.0
	if dt := cpu.total - s.prevCPU.total; cpu.total > s.prevCPU.total && dt > 0 {
		pct = round1(100 * float64(dt-(cpu.idle-s.prevCPU.idle)) / float64(dt))
	}
	procs := s.processes(now)
	net := s.netTotals()
	var rxKBps, txKBps float64
	if elapsed > 0 && net.rx >= s.prevNet.rx && net.tx >= s.prevNet.tx {
		rxKBps = round1(float64(net.rx-s.prevNet.rx) / 1024 / elapsed)
		txKBps = round1(float64(net.tx-s.prevNet.tx) / 1024 / elapsed)
	}
	s.prevCPU, s.prevNet, s.prevSampleAt = cpu, net, now
	l1, l5, l15 := s.loadAvg()
	memTotal, memUsed, swapTotal, swapUsed := s.memInfo()
	return contract.HostSnapshot{
		Hostname: s.hostname(), OS: s.osName(), Kernel: s.kernel(), Arch: goarch,
		CPUCores: s.cpuCores(), CPUPercent: pct, Load1: l1, Load5: l5, Load15: l15,
		MemTotalMi: memTotal, MemUsedMi: memUsed, SwapTotalMi: swapTotal, SwapUsedMi: swapUsed,
		UptimeSec: s.uptime(), NetRxKBps: rxKBps, NetTxKBps: txKBps, Disks: s.disks(), Processes: procs,
	}
}
