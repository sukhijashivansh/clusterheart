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

// journalFields are the only fields the agent reads; asking journalctl for
// just these (plus the cursor and timestamp it always adds) makes each line
// about a fifth of the size to parse.
const journalFields = "MESSAGE,PRIORITY,_SYSTEMD_UNIT,SYSLOG_IDENTIFIER"

// journalLine is the subset of a journalctl JSON line the agent uses. Values
// are raw because a field can be a string, a byte array or a list of values.
type journalLine struct {
	Cursor   string          `json:"__CURSOR"`
	Realtime json.RawMessage `json:"__REALTIME_TIMESTAMP"`
	Priority json.RawMessage `json:"PRIORITY"`
	Unit     json.RawMessage `json:"_SYSTEMD_UNIT"`
	Ident    json.RawMessage `json:"SYSLOG_IDENTIFIER"`
	Message  json.RawMessage `json:"MESSAGE"`
}

// readJournal returns entries after the cursor (or from the last hour on the
// first call) and the new cursor. Bounded to the most recent maxJournalLines.
// fields limits the output to journalFields (systemd 236+); err is set when
// journalctl failed without output, e.g. an older systemd rejecting it.
func readJournal(ctx context.Context, run runFunc, cursor string, fields bool) ([]journalEntry, string, error) {
	args := []string{"-o", "json", "--no-pager", "-n", strconv.Itoa(maxJournalLines)}
	if fields {
		args = append(args, "--output-fields="+journalFields)
	}
	if cursor != "" {
		args = append(args, "--after-cursor", cursor)
	} else {
		args = append(args, "--since", "-1h")
	}
	out, err := run(ctx, "journalctl", args...)
	if err != nil && len(out) == 0 {
		return nil, cursor, err
	}
	var entries []journalEntry
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var l journalLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if l.Cursor != "" {
			cursor = l.Cursor
		}
		e := journalEntry{message: rawString(l.Message), priority: 6}
		if p, err := strconv.Atoi(rawString(l.Priority)); err == nil {
			e.priority = p
		}
		if us, err := strconv.ParseInt(rawString(l.Realtime), 10, 64); err == nil {
			e.at = time.UnixMicro(us).UTC()
		}
		e.unit = rawString(l.Unit)
		if e.unit == "" {
			e.unit = rawString(l.Ident)
		}
		if e.message != "" {
			entries = append(entries, e)
		}
	}
	return entries, cursor, nil
}

// rawString decodes one journal field value.
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return journalString(v)
}

// journalString decodes a journal field: a string, or an array of bytes for
// binary/non-UTF8 messages.
func journalString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		if len(t) > 0 {
			if first, ok := t[0].(string); ok { // a field logged more than once
				return first
			}
		}
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
