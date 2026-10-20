// Clusterheart account server (MongoDB-backed).
//
// Handles sign-up, login, logout and sessions (bcrypt-hashed passwords, signed
// session tokens) plus an admin panel where the owner approves or revokes who
// may use the app. Users live in the `users` collection of the MongoDB
// database named by MONGO_DB (default "clusterheart").
//
// The Electron desktop app calls POST /api/login on launch: the response tells
// it whether this user has been granted permission.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type User struct {
	Email     string    `bson:"email" json:"email"`
	Name      string    `bson:"name" json:"name"`
	PassHash  string    `bson:"passHash" json:"-"`
	Approved  bool      `bson:"approved" json:"approved"`
	Created   time.Time `bson:"created" json:"created"`
	LastLogin time.Time `bson:"lastLogin" json:"lastLogin"`
	// SessionsValidAfter invalidates any token minted before it (force log out).
	SessionsValidAfter time.Time `bson:"sessionsValidAfter,omitempty" json:"-"`
}

type app struct {
	store       *store
	secret      []byte
	adminEmails map[string]bool
}

func (a *app) isAdmin(email string) bool {
	return a.adminEmails[strings.ToLower(strings.TrimSpace(email))]
}

// --- sessions ------------------------------------------------------------
// A session token is base64url(email|expiryRFC3339).hmac. Stateless: works as
// a cookie (web) and as a bearer token (Electron).

const sessionCookie = "ch_session"
const sessionTTL = 30 * 24 * time.Hour

func (a *app) mint(email string) string {
	now := time.Now()
	exp := now.Add(sessionTTL)
	// email|exp|iat - iat lets us reject tokens minted before a force-logout.
	msg := strings.ToLower(email) + "|" + exp.UTC().Format(time.RFC3339) + "|" + now.UTC().Format(time.RFC3339)
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString([]byte(msg)) + "." + hex.EncodeToString(mac.Sum(nil))
}

// parse verifies the token and returns the email and its issue time. Older
// two-field tokens (no iat) parse with a zero iat, so any force-logout stamp
// invalidates them - which is the safe direction.
func (a *app) parse(token string) (email string, iat time.Time, ok bool) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return "", time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", time.Time{}, false
	}
	mac := hmac.New(sha256.New, a.secret)
	mac.Write(raw)
	if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(parts[1])) {
		return "", time.Time{}, false
	}
	fields := strings.Split(string(raw), "|")
	if len(fields) < 2 {
		return "", time.Time{}, false
	}
	exp, err := time.Parse(time.RFC3339, fields[1])
	if err != nil || time.Now().After(exp) {
		return "", time.Time{}, false
	}
	if len(fields) >= 3 {
		if t, err := time.Parse(time.RFC3339, fields[2]); err == nil {
			iat = t
		}
	}
	return fields[0], iat, true
}

// sessionUser resolves the current user from a cookie or Authorization bearer.
func (a *app) sessionUser(r *http.Request) *User {
	token := ""
	if c, err := r.Cookie(sessionCookie); err == nil {
		token = c.Value
	}
	if token == "" {
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			token = strings.TrimPrefix(h, "Bearer ")
		}
	}
	if token == "" {
		return nil
	}
	email, iat, ok := a.parse(token)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	u, err := a.store.find(ctx, email)
	if err != nil || u == nil {
		return nil
	}
	// Token minted before a force-logout stamp is dead.
	if !u.SessionsValidAfter.IsZero() && iat.Before(u.SessionsValidAfter) {
		return nil
	}
	return u
}

// --- API handlers --------------------------------------------------------

func (a *app) handleSignup(w http.ResponseWriter, r *http.Request) {
	var body struct{ Name, Email, Password string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	name := strings.TrimSpace(body.Name)
	if !strings.Contains(email, "@") || len(email) < 5 {
		writeJSON(w, 400, map[string]any{"error": "Enter a valid email address."})
		return
	}
	if len(body.Password) < 8 {
		writeJSON(w, 400, map[string]any{"error": "Password must be at least 8 characters."})
		return
	}
	if name == "" {
		writeJSON(w, 400, map[string]any{"error": "Please enter your name."})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": "could not create account"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	err = a.store.insert(ctx, &User{
		Email:    email,
		Name:     name,
		PassHash: string(hash),
		Approved: false,
		Created:  time.Now(),
	})
	if err == errUserExists {
		writeJSON(w, 409, map[string]any{"error": "An account with this email already exists."})
		return
	}
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": "could not create account"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// isHTTPS reports whether the request reached us over TLS, either directly or
// via a terminating proxy (Caddy/Vercel set X-Forwarded-Proto). Used to mark
// the session cookie Secure in production while keeping local http dev working.
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (a *app) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct{ Email, Password string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	u, err := a.store.find(ctx, email)
	if err != nil {
		writeJSON(w, 503, map[string]any{"error": "service unavailable, try again"})
		return
	}
	if u == nil || bcrypt.CompareHashAndPassword([]byte(u.PassHash), []byte(body.Password)) != nil {
		writeJSON(w, 401, map[string]any{"error": "Incorrect email or password."})
		return
	}
	_ = a.store.setLastLogin(ctx, email)

	token := a.mint(email)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionTTL),
	})
	writeJSON(w, 200, map[string]any{
		"ok":       true,
		"approved": u.Approved,
		"name":     u.Name,
		"email":    u.Email,
		"admin":    a.isAdmin(email),
		"token":    token,
	})
}

func (a *app) handleSession(w http.ResponseWriter, r *http.Request) {
	u := a.sessionUser(r)
	if u == nil {
		writeJSON(w, 200, map[string]any{"valid": false})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	n := a.store.getNotice(ctx)
	resp := map[string]any{
		"valid":    true,
		"approved": u.Approved,
		"name":     u.Name,
		"email":    u.Email,
		"admin":    a.isAdmin(u.Email),
	}
	if n.Message != "" {
		resp["notice"] = n
	}
	// Latest shipped app version, so the desktop app can nudge users to update.
	if v := os.Getenv("LATEST_APP_VERSION"); v != "" {
		resp["latestVersion"] = v
	}
	writeJSON(w, 200, resp)
}

func (a *app) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *app) handleApprove(w http.ResponseWriter, r *http.Request) {
	me := a.sessionUser(r)
	if me == nil || !a.isAdmin(me.Email) {
		writeJSON(w, 403, map[string]any{"error": "forbidden"})
		return
	}
	var body struct {
		Email    string `json:"email"`
		Approved bool   `json:"approved"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := a.store.setApproved(ctx, body.Email, body.Approved); err != nil {
		writeJSON(w, 500, map[string]any{"error": "update failed"})
		return
	}
	// Revoking also kills any live session so the lockout is immediate.
	if !body.Approved {
		_ = a.store.invalidateSessions(ctx, body.Email)
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleForceLogout ends a user's active sessions without touching approval -
// they can sign back in if still approved.
func (a *app) handleForceLogout(w http.ResponseWriter, r *http.Request) {
	me := a.sessionUser(r)
	if me == nil || !a.isAdmin(me.Email) {
		writeJSON(w, 403, map[string]any{"error": "forbidden"})
		return
	}
	var body struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Email == "" {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := a.store.invalidateSessions(ctx, body.Email); err != nil {
		writeJSON(w, 500, map[string]any{"error": "update failed"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleNotice sets (or clears, when message is empty) the global broadcast
// notice that every app shows on its next heartbeat.
func (a *app) handleNotice(w http.ResponseWriter, r *http.Request) {
	me := a.sessionUser(r)
	if me == nil || !a.isAdmin(me.Email) {
		writeJSON(w, 403, map[string]any{"error": "forbidden"})
		return
	}
	var body struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := a.store.setNotice(ctx, strings.TrimSpace(body.Message)); err != nil {
		writeJSON(w, 500, map[string]any{"error": "update failed"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// --- page handlers -------------------------------------------------------

func (a *app) pageLogin(w http.ResponseWriter, r *http.Request) {
	if u := a.sessionUser(r); u != nil {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	render(w, loginTmpl, nil)
}

func (a *app) pageSignup(w http.ResponseWriter, r *http.Request) {
	render(w, signupTmpl, nil)
}

// pageHome handles "/" and any unmatched path. The marketing landing page now
// lives on Vercel (clusterheart.com); this server is accounts-only, so the root
// shows the sign-in page directly (a 200, so platform health checks pass) and
// every other unmatched path redirects to /login.
func (a *app) pageHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	a.pageLogin(w, r)
}

func (a *app) pageAccount(w http.ResponseWriter, r *http.Request) {
	u := a.sessionUser(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	render(w, accountTmpl, map[string]any{
		"Name":     u.Name,
		"Email":    u.Email,
		"Approved": u.Approved,
		"IsAdmin":  a.isAdmin(u.Email),
	})
}

func (a *app) pageAdmin(w http.ResponseWriter, r *http.Request) {
	me := a.sessionUser(r)
	if me == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if !a.isAdmin(me.Email) {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	list, err := a.store.list(ctx)
	if err != nil {
		http.Error(w, "could not load users", 500)
		return
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Approved != list[j].Approved {
			return !list[i].Approved // pending first
		}
		return list[i].Created.After(list[j].Created)
	})
	pending := 0
	for _, u := range list {
		if !u.Approved {
			pending++
		}
	}
	render(w, adminTmpl, map[string]any{"Users": list, "Me": me.Name, "Pending": pending, "Notice": a.store.getNotice(ctx).Message})
}

// --- wiring --------------------------------------------------------------

func post(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	}
}

// statusWriter captures the response status for request logging.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(c int) { s.status = c; s.ResponseWriter.WriteHeader(c) }

// logging logs every request with its method, path, status and duration so we
// can see whether requests actually arrive and how long they take.
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, sw.status, time.Since(start))
	})
}

const installScript = `#!/bin/bash
set -e
BASE="%s"

arch=$(uname -m)
if [ "$arch" = "arm64" ]; then
  FILE="Clusterheart-macOS-AppleSilicon.dmg"
else
  FILE="Clusterheart-macOS-Intel.dmg"
fi

echo "Downloading Clusterheart ($arch)..."
curl -# -L "$BASE/downloads/$FILE" -o /tmp/clusterheart-install.dmg

echo "Mounting installer..."
hdiutil attach /tmp/clusterheart-install.dmg -mountpoint /Volumes/ClusterheartInstall -quiet

echo "Preparing app..."
rm -rf /tmp/Clusterheart-stage.app
cp -r "/Volumes/ClusterheartInstall/Clusterheart.app" /tmp/Clusterheart-stage.app

hdiutil detach /Volumes/ClusterheartInstall -quiet
rm /tmp/clusterheart-install.dmg

echo "Signing app before install (prevents Gatekeeper scan on copy)..."
xattr -dr com.apple.quarantine /tmp/Clusterheart-stage.app 2>/dev/null || true
codesign --force --sign - /tmp/Clusterheart-stage.app 2>/dev/null || true

echo "Moving to /Applications..."
rm -rf /Applications/Clusterheart.app
mv /tmp/Clusterheart-stage.app /Applications/Clusterheart.app

echo ""
echo "Done! Open Clusterheart from /Applications or Spotlight (Cmd+Space)."
echo "Sign in at: %s/login"
`

func main() {
	loadDotEnv(".env")

	addr := flag.String("addr", defaultAddr(), "listen address")
	flag.Parse()

	st, err := openStore(context.Background())
	if err != nil {
		log.Fatalf("mongo: %v", err)
	}
	log.Printf("connected to MongoDB (db=%s)", envOr("MONGO_DB", "clusterheart"))

	a := &app{
		store:       st,
		secret:      loadOrCreateSecret(envOr("SECRET_FILE", "secret.key")),
		adminEmails: parseAdminEmails(os.Getenv("ADMIN_EMAILS")),
	}
	if len(a.adminEmails) == 0 {
		log.Println("WARNING: ADMIN_EMAILS is empty - nobody can reach /admin. Set it in server/.env")
	}

	// Daily hard-purge of usage history (on top of the 8h TTL).
	go st.usagePurgeLoop()

	// Per-IP rate limiters for the sensitive auth endpoints.
	loginRL := newRateLimiter(10, time.Minute)      // login / token exchange
	signupRL := newRateLimiter(5, 10*time.Minute)   // account creation
	resetRL := newRateLimiter(5, 15*time.Minute)    // forgot / reset password

	mux := http.NewServeMux()
	mux.HandleFunc("/login", a.pageLogin)
	mux.HandleFunc("/signup", a.pageSignup)
	mux.HandleFunc("/account", a.pageAccount)
	mux.HandleFunc("/admin", a.pageAdmin)
	mux.HandleFunc("/forgot", a.pageForgot)
	mux.HandleFunc("/reset", a.pageReset)
	mux.HandleFunc("/api/signup", signupRL.wrap("signup", post(a.handleSignup)))
	mux.HandleFunc("/api/login", loginRL.wrap("login", post(a.handleLogin)))
	mux.HandleFunc("/api/logout", post(a.handleLogout))
	mux.HandleFunc("/api/session", a.handleSession)
	mux.HandleFunc("/api/forgot", resetRL.wrap("forgot", post(a.handleForgot)))
	mux.HandleFunc("/api/reset", resetRL.wrap("reset", post(a.handleReset)))
	mux.HandleFunc("/api/admin/approve", post(a.handleApprove))
	mux.HandleFunc("/api/admin/forcelogout", post(a.handleForceLogout))
	mux.HandleFunc("/api/admin/notice", post(a.handleNotice))
	mux.HandleFunc("/auth/google/start", a.googleStart)
	mux.HandleFunc("/auth/google/callback", a.googleCallback)
	mux.HandleFunc("/api/auth/exchange", loginRL.wrap("exchange", post(a.handleExchange)))
	mux.HandleFunc("/api/usage/ingest", post(a.handleUsageIngest))
	mux.HandleFunc("/api/usage/aggregate", a.handleUsageAggregate)

	// Serve the desktop-app installers straight from this server (no GitHub
	// release needed - the source stays private). Drop the .dmg/.exe/.AppImage
	// files into DOWNLOADS_DIR (default: ./downloads next to the binary).
	downloadsDir := os.Getenv("DOWNLOADS_DIR")
	if downloadsDir == "" {
		downloadsDir = "downloads"
	}
	mux.Handle("/downloads/", http.StripPrefix("/downloads/",
		http.FileServer(http.Dir(downloadsDir))))

	mux.HandleFunc("/install.sh", func(w http.ResponseWriter, r *http.Request) {
		scheme := "https"
		if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
			scheme = p
		} else if r.TLS == nil {
			scheme = "http"
		}
		base := scheme + "://" + r.Host
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, installScript, base, base)
	})

	mux.HandleFunc("/", a.pageHome)

	log.Printf("clusterheart account server on http://%s (downloads: %s)", *addr, downloadsDir)
	log.Fatal(http.ListenAndServe(*addr, logging(securityHeaders(mux))))
}

// --- helpers -------------------------------------------------------------

func loadOrCreateSecret(path string) []byte {
	if env := os.Getenv("SESSION_SECRET"); env != "" {
		return []byte(env)
	}
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b
	}
	b := make([]byte, 48)
	_, _ = rand.Read(b)
	_ = os.WriteFile(path, b, 0o600) // persist so sessions survive restarts
	return b
}

func parseAdminEmails(s string) map[string]bool {
	out := map[string]bool{}
	for _, e := range strings.Split(s, ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			out[e] = true
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// defaultAddr picks the listen address. If a PORT env var is injected we bind
// 0.0.0.0:$PORT; otherwise we fall back to ADDR (or 127.0.0.1:9000) so dev
// behaviour is unchanged. On AWS the systemd unit sets ADDR=0.0.0.0:9000.
func defaultAddr() string {
	if p := os.Getenv("PORT"); p != "" {
		return "0.0.0.0:" + p
	}
	return envOr("ADDR", "127.0.0.1:9000")
}
