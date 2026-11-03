package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRunHTTPPhases(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("User-Agent") != "clusterheart-latency-check" {
			w.WriteHeader(400)
			return
		}
		time.Sleep(30 * time.Millisecond) // server think time
		w.Write([]byte(strings.Repeat("x", 5000)))
	}))
	defer ts.Close()
	res := Run(context.Background(), ts.URL+"/health", 3)
	if res.Count != 3 || res.OKCount != 3 || len(res.Samples) != 3 {
		t.Fatalf("result: %+v", res)
	}
	for _, s := range res.Samples {
		if s.Status != 200 || s.Bytes != 5000 || s.TLSms != 0 || s.WaitMs < 29 || s.FirstByte < s.WaitMs || s.TotalMs < s.FirstByte {
			t.Fatalf("sample: %+v", s)
		}
	}
	if res.Wait.Min < 29 || res.Wait.Avg < res.Wait.Min || res.Wait.Max < res.Wait.Avg || res.TLS.Max != 0 {
		t.Fatalf("stats: wait %+v tls %+v", res.Wait, res.TLS)
	}
}

func TestRunHTTPSFreshConnectionEachSample(t *testing.T) {
	conns := 0
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	ts.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns++
		}
	}
	ts.StartTLS() // self-signed: must still be timed
	defer ts.Close()
	res := Run(context.Background(), ts.URL+"/", 4)
	if res.OKCount != 4 || res.TLS.Min <= 0 || conns != 4 {
		t.Fatalf("ok=%d tls=%+v conns=%d samples=%+v", res.OKCount, res.TLS, conns, res.Samples)
	}
}

func TestRunDoesNotFollowRedirectsAndCapsSamples(t *testing.T) {
	hits := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "http://example.invalid/elsewhere", http.StatusFound)
	}))
	defer ts.Close()
	res := Run(context.Background(), ts.URL+"/", 100)
	if res.Count != MaxSamples || hits != MaxSamples || res.Samples[0].Status != 302 {
		t.Fatalf("count=%d hits=%d first=%+v", res.Count, hits, res.Samples[0])
	}
}

func TestRunErrorsArePlain(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close() // nothing listens here now
	res := Run(context.Background(), "http://"+addr+"/", 2)
	if res.OKCount != 0 || !strings.Contains(res.Samples[0].Err, "connection refused") {
		t.Fatalf("refused: %+v", res.Samples)
	}
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer plain.Close()
	res = Run(context.Background(), "https://"+strings.TrimPrefix(plain.URL, "http://")+"/", 1)
	if !strings.Contains(res.Samples[0].Err, "plain http") {
		t.Fatalf("https to http port: %+v", res.Samples)
	}
}
