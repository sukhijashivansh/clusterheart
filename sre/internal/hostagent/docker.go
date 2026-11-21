package hostagent

import (
	"encoding/json"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"clusterheart/sre/internal/contract"
	"clusterheart/sre/internal/redact"
)

const (
	// dockerStateFile is written by a root-owned timer (see install-host.sh):
	// the Docker socket is root-equivalent, so the agent never gets access to it.
	dockerStateFile = "/run/clusterheart/docker.json"
	dockerStale     = 2 * time.Minute // older than this: the timer or Docker stopped
	maxContainers   = 100
	maxDockerFile   = 1 << 20
)

// readContainers returns the containers in the state file, running ones first.
// A missing or stale file reads as none.
func readContainers(path string, now time.Time) []contract.HostContainer {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || now.Sub(st.ModTime()) > dockerStale {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(f, maxDockerFile))
	if err != nil {
		return nil
	}
	return parseContainers(b)
}

// parseContainers reads one JSON object per line and skips lines it can't parse.
func parseContainers(b []byte) []contract.HostContainer {
	var out []contract.HostContainer
	for _, l := range strings.Split(string(b), "\n") {
		var c contract.HostContainer
		if json.Unmarshal([]byte(l), &c) != nil || c.ID == "" {
			continue
		}
		c.Name, c.Image = redact.String(c.Name), redact.String(c.Image)
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if ri, rj := out[i].State == "running", out[j].State == "running"; ri != rj {
			return ri
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > maxContainers {
		out = out[:maxContainers]
	}
	return out
}
