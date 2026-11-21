package hostagent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseContainers(t *testing.T) {
	out := `{"id":"bbb","name":"old-job","image":"busybox","state":"exited","status":"Exited (1) 2 hours ago","ports":""}
garbage
{"id":"aaa","name":"vllm","image":"vllm/vllm-openai:latest","state":"running","status":"Up 2 days (healthy)","ports":"0.0.0.0:8000->8000/tcp"}
`
	c := parseContainers([]byte(out))
	if len(c) != 2 || c[0].Name != "vllm" || c[1].Name != "old-job" {
		t.Fatalf("containers: %+v", c)
	}
	if c[0].Ports != "0.0.0.0:8000->8000/tcp" || c[0].Status != "Up 2 days (healthy)" {
		t.Fatalf("fields: %+v", c[0])
	}
}

func TestReadContainersIgnoresStaleFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "docker.json")
	write(t, p, `{"id":"aaa","name":"vllm","image":"x","state":"running","status":"Up","ports":""}`+"\n")
	now := time.Now()
	if got := readContainers(p, now); len(got) != 1 {
		t.Fatalf("fresh file: %+v", got)
	}
	if got := readContainers(p, now.Add(dockerStale+time.Minute)); got != nil {
		t.Fatalf("stale file: %+v", got)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if got := readContainers(p, now); got != nil {
		t.Fatalf("missing file: %+v", got)
	}
}
