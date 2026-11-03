// Package probe times HTTP GET requests the way curl -w does: TCP connect,
// TLS handshake, server wait (time to first byte) and transfer. Each request
// uses a fresh connection so every sample includes the handshakes. Redirects
// are not followed, bodies are read (up to a cap) and discarded, and nothing
// about the response except status and size is kept.
package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	"clusterheart/sre/internal/contract"
)

const (
	MaxSamples   = 20
	dialTimeout  = 5 * time.Second
	reqTimeout   = 10 * time.Second
	maxBodyBytes = 1 << 20
)

func ms(d time.Duration) float64 { return math.Round(float64(d.Microseconds())/10) / 100 }

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func client() *http.Client {
	tr := &http.Transport{
		Proxy:               nil, // straight to the target, never through a proxy
		DialContext:         (&net.Dialer{Timeout: dialTimeout}).DialContext,
		TLSHandshakeTimeout: dialTimeout,
		// Pods often serve self-signed certificates; only timing is measured.
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: reqTimeout,
		DisableCompression:    true,
	}
	return &http.Client{Transport: tr, Timeout: reqTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// one times a single GET.
func one(ctx context.Context, c *http.Client, url string) contract.ProbeSample {
	var s contract.ProbeSample
	var connStart, tlsStart, firstByte time.Time
	var connDur, tlsDur time.Duration
	trace := &httptrace.ClientTrace{
		ConnectStart:         func(_, _ string) { connStart = time.Now() },
		ConnectDone:          func(_, _ string, _ error) { connDur = time.Since(connStart) },
		TLSHandshakeStart:    func() { tlsStart = time.Now() },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { tlsDur = time.Since(tlsStart) },
		GotFirstResponseByte: func() { firstByte = time.Now() },
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, url, nil)
	if err != nil {
		s.Err = err.Error()
		return s
	}
	req.Header.Set("User-Agent", "clusterheart-latency-check")
	start := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		s.Err = describe(err)
		s.ConnectMs, s.TLSms = ms(connDur), ms(tlsDur)
		return s
	}
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
	resp.Body.Close()
	end := time.Now()
	s.Status, s.Bytes = resp.StatusCode, n
	s.ConnectMs, s.TLSms, s.TotalMs = ms(connDur), ms(tlsDur), ms(end.Sub(start))
	if !firstByte.IsZero() {
		s.FirstByte = ms(firstByte.Sub(start))
		s.WaitMs = round2(math.Max(0, s.FirstByte-s.ConnectMs-s.TLSms))
		s.TransferMs = round2(math.Max(0, s.TotalMs-s.FirstByte))
	}
	return s
}

// describe turns network errors into short, plain messages.
func describe(err error) string {
	var ne net.Error
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"):
		return "connection refused: nothing is listening on that port"
	case errors.As(err, &ne) && ne.Timeout():
		return "timed out (a network policy may block the agent, or the app is not answering)"
	case strings.Contains(msg, "server gave HTTP response to HTTPS client"), strings.Contains(msg, "first record does not look like a TLS handshake"):
		return "the port speaks plain http, not https"
	case strings.Contains(msg, "malformed HTTP response"):
		return "the port did not answer with HTTP (try https, or another port)"
	}
	if i := strings.LastIndex(msg, ": "); i >= 0 && len(msg)-i < 120 {
		return msg[i+2:]
	}
	if len(msg) > 160 {
		msg = msg[:160]
	}
	return msg
}

func stat(vals []float64) contract.ProbeStat {
	if len(vals) == 0 {
		return contract.ProbeStat{}
	}
	st := contract.ProbeStat{Min: vals[0], Max: vals[0]}
	sum := 0.0
	for _, v := range vals {
		st.Min, st.Max = math.Min(st.Min, v), math.Max(st.Max, v)
		sum += v
	}
	st.Avg = round2(sum / float64(len(vals)))
	return st
}

// Run times n sequential GETs to url and summarises the successful ones.
func Run(ctx context.Context, url string, n int) contract.ProbeResult {
	n = min(max(n, 1), MaxSamples)
	res := contract.ProbeResult{URL: url, Count: n}
	c := client()
	var conn, tl, wait, xfer, fb, total []float64
	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			res.Samples = append(res.Samples, contract.ProbeSample{Err: "stopped: the check took too long"})
			continue
		}
		s := one(ctx, c, url)
		res.Samples = append(res.Samples, s)
		if s.Err != "" {
			continue
		}
		res.OKCount++
		conn, tl, wait, xfer = append(conn, s.ConnectMs), append(tl, s.TLSms), append(wait, s.WaitMs), append(xfer, s.TransferMs)
		fb, total = append(fb, s.FirstByte), append(total, s.TotalMs)
	}
	res.Connect, res.TLS, res.Wait, res.Transfer, res.FirstByte, res.Total = stat(conn), stat(tl), stat(wait), stat(xfer), stat(fb), stat(total)
	return res
}
