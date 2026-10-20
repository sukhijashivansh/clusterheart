package main

// Google OAuth sign-in. New Google users are created pending (same approval
// gate as email signup) unless AUTO_APPROVE_DOMAIN matches their email domain.
//
// Web flow:    /auth/google/start -> Google -> /auth/google/callback -> cookie -> /account
// Desktop flow: app opens the system browser to /auth/google/start?desktop=1;
//               the callback hands a one-time code back via the clusterheart://
//               protocol, which the app swaps for a session at /api/auth/exchange.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	googleAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL    = "https://oauth2.googleapis.com/token"
	googleUserInfoURL = "https://openidconnect.googleapis.com/v1/userinfo"
	oauthCookie       = "g_oauth"
)

func googleEnabled() bool {
	return os.Getenv("GOOGLE_CLIENT_ID") != "" && os.Getenv("GOOGLE_CLIENT_SECRET") != ""
}

func publicBaseURL() string {
	if v := os.Getenv("PUBLIC_BASE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://www.clusterheart.com"
}

func randToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// autoApprove approves a Google sign-in automatically only if its email domain
// matches AUTO_APPROVE_DOMAIN (unset -> everyone stays pending).
func autoApprove(email string) bool {
	dom := strings.ToLower(strings.TrimSpace(os.Getenv("AUTO_APPROVE_DOMAIN")))
	return dom != "" && strings.HasSuffix(strings.ToLower(email), "@"+dom)
}

// one-time codes for the desktop (system-browser) flow: code -> session token.
type codeEntry struct {
	token string
	exp   time.Time
}

var codeStore = struct {
	mu sync.Mutex
	m  map[string]codeEntry
}{m: map[string]codeEntry{}}

func putCode(token string) string {
	code := randToken(24)
	codeStore.mu.Lock()
	defer codeStore.mu.Unlock()
	codeStore.m[code] = codeEntry{token: token, exp: time.Now().Add(5 * time.Minute)}
	return code
}

func takeCode(code string) (string, bool) {
	codeStore.mu.Lock()
	defer codeStore.mu.Unlock()
	e, ok := codeStore.m[code]
	if !ok {
		return "", false
	}
	delete(codeStore.m, code)
	if time.Now().After(e.exp) {
		return "", false
	}
	return e.token, true
}

func (a *app) googleStart(w http.ResponseWriter, r *http.Request) {
	if !googleEnabled() {
		http.Error(w, "Google sign-in is not configured.", http.StatusServiceUnavailable)
		return
	}
	mode := "w"
	if r.URL.Query().Get("desktop") == "1" {
		mode = "d"
	}
	state := randToken(18) + ":" + mode
	http.SetCookie(w, &http.Cookie{
		Name: oauthCookie, Value: state, Path: "/", HttpOnly: true,
		Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode, Expires: time.Now().Add(10 * time.Minute),
	})
	q := url.Values{}
	q.Set("client_id", os.Getenv("GOOGLE_CLIENT_ID"))
	q.Set("redirect_uri", publicBaseURL()+"/auth/google/callback")
	q.Set("response_type", "code")
	q.Set("scope", "openid email profile")
	q.Set("state", state)
	q.Set("access_type", "online")
	q.Set("prompt", "select_account")
	http.Redirect(w, r, googleAuthURL+"?"+q.Encode(), http.StatusSeeOther)
}

func (a *app) googleCallback(w http.ResponseWriter, r *http.Request) {
	if !googleEnabled() {
		http.Error(w, "Google sign-in is not configured.", http.StatusServiceUnavailable)
		return
	}
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	c, err := r.Cookie(oauthCookie)
	http.SetCookie(w, &http.Cookie{Name: oauthCookie, Value: "", Path: "/", MaxAge: -1})
	if err != nil || c.Value == "" || c.Value != state || code == "" {
		http.Error(w, "Sign-in failed (bad state). Please try again.", http.StatusBadRequest)
		return
	}
	mode := "w"
	if parts := strings.SplitN(state, ":", 2); len(parts) == 2 {
		mode = parts[1]
	}

	email, name, err := a.googleExchange(r.Context(), code)
	if err != nil {
		http.Error(w, "Google sign-in failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	u, _ := a.store.find(ctx, email)
	if u == nil {
		if err := a.store.insert(ctx, &User{
			Email: email, Name: name, PassHash: randToken(24), // unusable password
			Approved: autoApprove(email), Created: time.Now(),
		}); err != nil && err != errUserExists {
			http.Error(w, "could not create account", http.StatusInternalServerError)
			return
		}
		u, _ = a.store.find(ctx, email)
		if u == nil {
			http.Error(w, "could not create account", http.StatusInternalServerError)
			return
		}
	}
	_ = a.store.setLastLogin(ctx, email)
	token := a.mint(email)

	if mode == "d" {
		renderDesktopHandback(w, putCode(token))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode, Expires: time.Now().Add(sessionTTL),
	})
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// googleExchange swaps the auth code for tokens and returns the verified email + name.
func (a *app) googleExchange(ctx context.Context, code string) (email, name string, err error) {
	form := url.Values{}
	form.Set("code", code)
	form.Set("client_id", os.Getenv("GOOGLE_CLIENT_ID"))
	form.Set("client_secret", os.Getenv("GOOGLE_CLIENT_SECRET"))
	form.Set("redirect_uri", publicBaseURL()+"/auth/google/callback")
	form.Set("grant_type", "authorization_code")
	req, _ := http.NewRequestWithContext(ctx, "POST", googleTokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		return "", "", fmt.Errorf("token exchange %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&tok); err != nil || tok.AccessToken == "" {
		return "", "", fmt.Errorf("no access token in response")
	}

	ireq, _ := http.NewRequestWithContext(ctx, "GET", googleUserInfoURL, nil)
	ireq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	ires, err := http.DefaultClient.Do(ireq)
	if err != nil {
		return "", "", err
	}
	defer ires.Body.Close()
	if ires.StatusCode != 200 {
		return "", "", fmt.Errorf("userinfo request failed (%d)", ires.StatusCode)
	}
	var info struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := json.NewDecoder(ires.Body).Decode(&info); err != nil {
		return "", "", err
	}
	if info.Email == "" || !info.EmailVerified {
		return "", "", fmt.Errorf("Google account email is not verified")
	}
	nm := info.Name
	if nm == "" {
		nm = strings.SplitN(info.Email, "@", 2)[0]
	}
	return strings.ToLower(info.Email), nm, nil
}

// handleExchange swaps a one-time desktop code for the session token + status.
func (a *app) handleExchange(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Code == "" {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	token, ok := takeCode(body.Code)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "code expired, try again"})
		return
	}
	email, _, ok := a.parse(token)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "invalid session"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	u, err := a.store.find(ctx, email)
	if err != nil || u == nil {
		writeJSON(w, 401, map[string]any{"error": "invalid session"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "token": token, "approved": u.Approved, "name": u.Name})
}

var desktopHandbackTmpl = template.Must(template.New("hb").Parse(`<!DOCTYPE html><html><head><meta charset="utf-8">
<title>Clusterheart</title><meta name="viewport" content="width=device-width,initial-scale=1">
<style>body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:#fff;color:#171717;display:flex;min-height:100vh;align-items:center;justify-content:center;text-align:center;margin:0}
.card{max-width:360px;padding:40px}h1{font-size:20px;margin:0 0 8px}p{color:#525252;font-size:14px;line-height:1.6}
a.btn{display:inline-block;margin-top:18px;padding:12px 20px;background:#000;color:#fff;text-decoration:none;font-weight:600;font-size:14px}</style>
<script>window.location.replace("clusterheart://auth?code={{.}}");</script></head>
<body><div class="card"><h1>Signed in</h1><p>Returning you to the Clusterheart app&hellip; If it doesn't come forward, click below.</p>
<a class="btn" href="clusterheart://auth?code={{.}}">Open Clusterheart</a></div></body></html>`))

func renderDesktopHandback(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = desktopHandbackTmpl.Execute(w, code)
}
