package hostagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"clusterheart/sre/internal/contract"
	"clusterheart/sre/internal/redact"
)

var goarch = runtime.GOARCH

// runFunc runs a read-only command and returns its stdout. Tests replace it.
type runFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return exec.CommandContext(cctx, name, args...).Output()
}

// failedUnits lists systemd units in the failed state.
func failedUnits(ctx context.Context, run runFunc) []contract.HostUnit {
	out, err := run(ctx, "systemctl", "list-units", "--state=failed", "--no-legend", "--plain", "--no-pager")
	if err != nil {
		return nil
	}
	var units []contract.HostUnit
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		// UNIT LOAD ACTIVE SUB DESCRIPTION...
		f := strings.Fields(strings.TrimLeft(sc.Text(), "● "))
		if len(f) == 0 {
			continue
		}
		u := contract.HostUnit{Name: f[0]}
		if len(f) > 4 {
			u.Description = strings.Join(f[4:], " ")
		}
		units = append(units, u)
	}
	return units
}

// unitEnv returns the literal Environment= settings of running services, as
// unit -> []"NAME=value".
func unitEnv(ctx context.Context, run runFunc) map[string][]string {
	out, err := run(ctx, "systemctl", "show", "--type=service", "--state=running", "--property=Id,Environment", "--no-pager")
	if err != nil {
		return nil
	}
	res := map[string][]string{}
	unit := ""
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		l := sc.Text()
		switch {
		case l == "":
			unit = ""
		case strings.HasPrefix(l, "Id="):
			unit = strings.TrimPrefix(l, "Id=")
		case strings.HasPrefix(l, "Environment=") && unit != "":
			for _, kv := range splitEnv(strings.TrimPrefix(l, "Environment=")) {
				res[unit] = append(res[unit], kv)
			}
		}
	}
	return res
}

// splitEnv splits systemd's space-separated Environment value, honouring
// double quotes around assignments.
func splitEnv(s string) []string {
	var out []string
	var cur strings.Builder
	inQ := false
	for _, r := range s {
		switch {
		case r == '"':
			inQ = !inQ
		case r == ' ' && !inQ:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// journalEntry is one parsed journal line.
type journalEntry struct {
	at       time.Time
	unit     string
	message  string
	priority int
}

// readJournal returns entries after the cursor (or from the last hour on the
// first call) and the new cursor. Bounded to the most recent maxJournalLines.
func readJournal(ctx context.Context, run runFunc, cursor string) ([]journalEntry, string) {
	args := []string{"-o", "json", "--no-pager", "-n", strconv.Itoa(maxJournalLines)}
	if cursor != "" {
		args = append(args, "--after-cursor", cursor)
	} else {
		args = append(args, "--since", "-1h")
	}
	out, err := run(ctx, "journalctl", args...)
	if err != nil && len(out) == 0 {
		return nil, cursor
	}
	var entries []journalEntry
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var raw map[string]any
		if json.Unmarshal(sc.Bytes(), &raw) != nil {
			continue
		}
		if c, ok := raw["__CURSOR"].(string); ok {
			cursor = c
		}
		e := journalEntry{message: journalString(raw["MESSAGE"]), priority: 6}
		if p, err := strconv.Atoi(journalString(raw["PRIORITY"])); err == nil {
			e.priority = p
		}
		if us, err := strconv.ParseInt(journalString(raw["__REALTIME_TIMESTAMP"]), 10, 64); err == nil {
			e.at = time.UnixMicro(us).UTC()
		}
		e.unit = journalString(raw["_SYSTEMD_UNIT"])
		if e.unit == "" {
			e.unit = journalString(raw["SYSLOG_IDENTIFIER"])
		}
		if e.message != "" {
			entries = append(entries, e)
		}
	}
	return entries, cursor
}

// journalString decodes a journal field: a string, or an array of bytes for
// binary/non-UTF8 messages.
func journalString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		b := make([]byte, 0, len(t))
		for _, x := range t {
			if f, ok := x.(float64); ok {
				b = append(b, byte(f))
			}
		}
		return string(b)
	}
	return ""
}

// redactedLogLine turns an error-level journal entry into a snapshot line.
func redactedLogLine(e journalEntry) contract.HostLogLine {
	msg := redact.String(e.message)
	if len(msg) > 400 {
		msg = msg[:400] + "…"
	}
	return contract.HostLogLine{Time: e.at, Unit: e.unit, Message: msg}
}
