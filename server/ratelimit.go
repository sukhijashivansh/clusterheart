package main

// A small in-memory, per-IP fixed-window rate limiter for the auth endpoints,
// plus a response-header middleware that applies sane security headers.

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type rlBucket struct {
	count int
	reset time.Time
}

type rateLimiter struct {
	mu     sync.Mutex
	m      map[string]*rlBucket
	max    int
	window time.Duration
}

func newRateLimiter(max int, window time.Duration) *rateLimiter {
	rl := &rateLimiter{m: map[string]*rlBucket{}, max: max, window: window}
	go rl.sweep()
	return rl
}

// sweep periodically drops expired buckets so the map can't grow forever.
func (rl *rateLimiter) sweep() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		rl.mu.Lock()
		for k, b := range rl.m {
			if now.After(b.reset) {
				delete(rl.m, k)
			}
		}
		rl.mu.Unlock()
	}
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	b := rl.m[key]
	if b == nil || now.After(b.reset) {
		rl.m[key] = &rlBucket{count: 1, reset: now.Add(rl.window)}
		return true
	}
	if b.count >= rl.max {
		return false
	}
	b.count++
	return true
}

// wrap limits a handler per client IP under the given bucket name.
func (rl *rateLimiter) wrap(name string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow(name + "|" + clientIP(r)) {
			w.Header().Set("Retry-After", strconv.Itoa(int(rl.window.Seconds())))
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "Too many attempts. Please wait a bit and try again."})
			return
		}
		h(w, r)
	}
}

// clientIP resolves the real client IP behind the Vercel -> Caddy -> Go proxy
// chain. Vercel carries the original browser IP in X-Vercel-Forwarded-For (and
// X-Real-IP); X-Forwarded-For is the fallback for direct origin hits.
func clientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Vercel-Forwarded-For")); v != "" {
		return firstIP(v)
	}
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return firstIP(v)
	}
	if v := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); v != "" {
		return firstIP(v)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func firstIP(xff string) string {
	if i := strings.IndexByte(xff, ','); i > 0 {
		return strings.TrimSpace(xff[:i])
	}
	return strings.TrimSpace(xff)
}

// securityHeaders applies security response headers to every response. CSP is
// tuned for the account pages (inline styles/scripts + Google Fonts).
func securityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; " +
		"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; " +
		"font-src 'self' https://fonts.gstatic.com; " +
		"script-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; " +
		"connect-src 'self'; " +
		"frame-ancestors 'none'; base-uri 'self'; form-action 'self'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		h.Set("Content-Security-Policy", csp)
		next.ServeHTTP(w, r)
	})
}
