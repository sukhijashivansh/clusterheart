// Clusterheart account server (MongoDB-backed).
//
// Serves the web dashboard (/app) and everything around it: sign-up, login,
// sessions (bcrypt-hashed passwords, signed session cookies), password reset,
// teams, and the admin panel where the owner approves or revokes who may use
// the app. Cluster and server data comes from the SRE control plane (cpDo).
// Users live in the `users` collection of the MongoDB database named by
// MONGO_DB (default "clusterheart").
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
	// LastSeen is refreshed when the dashboard opens (see pageApp), so the
	// admin can see who's active.
	LastSeen time.Time `bson:"lastSeen,omitempty" json:"lastSeen"`
	// SessionsValidAfter invalidates any token minted before it (force log out).
	SessionsValidAfter time.Time `bson:"sessionsValidAfter,omitempty" json:"-"`
	// Workspace is the owner email of the team workspace this user opens;
	// empty means their own (see team.go).
	Workspace string `bson:"workspace,omitempty" json:"-"`
}

type app struct {
	store       *store
	secret      []byte
	adminEmails map[string]bool
}

func (a *app) isAdmin(email string) bool {
	return a.adminEmails[strings.ToLower(strings.TrimSpace(email))]
}

// autoApprove reports whether new/logging-in accounts should be approved
// automatically (set AUTO_APPROVE=true to skip the manual admin gate).
func autoApprove() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AUTO_APPROVE"))) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// --- sessions ------------------------------------------------------------
// A session token is base64url(email|expiry|issuedAt).hmac, kept in an
// HttpOnly cookie. Stateless apart from the per-user force-logout stamp.

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
	nu := &User{
		Email:    email,
		Name:     name,
		PassHash: string(hash),
		Approved: autoApprove(),
		Created:  time.Now(),
	}
	err = a.store.insert(ctx, nu)
	if err == errUserExists {
		writeJSON(w, 409, map[string]any{"error": "An account with this email already exists."})
		return
	}
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": "could not create account"})
		return
	}
	// Someone a team owner added: approved now, opening the team's workspace.
	a.joinInvitedTeam(ctx, nu)
	writeJSON(w, 200, map[string]any{"ok": true, "approved": nu.Approved})
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
	// People on an approved owner's team are approved and open that workspace.
	a.joinInvitedTeam(ctx, u)

	// Auto-approve on login (covers accounts created before the flag was on).
	if !u.Approved && autoApprove() {
		_ = a.store.setApproved(ctx, email, true)
		u.Approved = true
	}

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
	})
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
	initial := "?"
	for _, r := range strings.TrimSpace(u.Name + u.Email) {
		initial = strings.ToUpper(string(r))
		break
	}
	render(w, accountTmpl, map[string]any{
		"Initial":  initial,
		"Name":     u.Name,
		"Email":    u.Email,
		"Approved": u.Approved,
		"IsAdmin":  a.isAdmin(u.Email),
	})
}

// adminGuard returns the signed-in admin, or redirects and returns nil.
func (a *app) adminGuard(w http.ResponseWriter, r *http.Request) *User {
	me := a.sessionUser(r)
	if me == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return nil
	}
	if !a.isAdmin(me.Email) {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return nil
	}
	return me
}

// pageAdmin is the Users page (/admin).
func (a *app) pageAdmin(w http.ResponseWriter, r *http.Request) {
	me := a.adminGuard(w, r)
	if me == nil {
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
	rows := a.adminRows(list)
	totC, totS := 0, 0
	for _, r := range rows {
		totC += r.Clusters
		totS += r.Servers
	}
	render(w, adminUsersTmpl, map[string]any{"Users": rows, "Me": me.Name, "Pending": pending, "TotalClusters": totC, "TotalServers": totS})
}

// pageAdminBroadcast is the Broadcast page (/admin/broadcast).
func (a *app) pageAdminBroadcast(w http.ResponseWriter, r *http.Request) {
	me := a.adminGuard(w, r)
	if me == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	render(w, adminBroadcastTmpl, map[string]any{"Me": me.Name, "Notice": a.store.getNotice(ctx).Message})
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

	// Per-IP rate limiters for the sensitive auth endpoints.
	loginRL := newRateLimiter(10, time.Minute)    // login / token exchange
	signupRL := newRateLimiter(5, 10*time.Minute) // account creation
	resetRL := newRateLimiter(5, 15*time.Minute)  // forgot / reset password

	mux := http.NewServeMux()
	mux.HandleFunc("/login", a.pageLogin)
	mux.HandleFunc("/signup", a.pageSignup)
	mux.HandleFunc("/account", a.pageAccount)
	mux.HandleFunc("/app", a.pageApp)
	mux.HandleFunc("/admin", a.pageAdmin)
	mux.HandleFunc("/admin/broadcast", a.pageAdminBroadcast)
	mux.HandleFunc("/forgot", a.pageForgot)
	mux.HandleFunc("/reset", a.pageReset)
	mux.HandleFunc("/api/signup", signupRL.wrap("signup", post(a.handleSignup)))
	mux.HandleFunc("/api/login", loginRL.wrap("login", post(a.handleLogin)))
	mux.HandleFunc("/api/logout", post(a.handleLogout))
	mux.HandleFunc("/api/forgot", resetRL.wrap("forgot", post(a.handleForgot)))
	mux.HandleFunc("/api/reset", resetRL.wrap("reset", post(a.handleReset)))
	mux.HandleFunc("/api/admin/approve", post(a.handleApprove))
	mux.HandleFunc("/api/admin/forcelogout", post(a.handleForceLogout))
	mux.HandleFunc("/api/admin/notice", post(a.handleNotice))
	mux.HandleFunc("/api/team", a.handleTeam)
	mux.HandleFunc("POST /api/workspace", post(a.handleWorkspace))
	mux.HandleFunc("/api/app/clusters", a.handleAppClusters)
	mux.HandleFunc("POST /api/app/clusters/{id}/connect", a.handleAppClusterConnect)
	mux.HandleFunc("DELETE /api/app/clusters/{id}", a.handleAppClusterDelete)
	mux.HandleFunc("POST /api/app/clusters/{id}/rename", a.handleAppClusterRename)
	mux.HandleFunc("POST /api/app/clusters/{id}/environment", a.handleAppClusterSetEnv)
	mux.HandleFunc("GET /api/app/clusters/{id}/sre", a.handleAppClusterSRE)
	mux.HandleFunc("GET /api/app/clusters/{id}/usage", a.handleAppClusterUsage)
	mux.HandleFunc("GET /api/app/clusters/{id}/metrics", a.handleAppServerMetrics)
	mux.HandleFunc("POST /api/app/clusters/{id}/probes", a.handleAppStartProbe)
	mux.HandleFunc("GET /api/app/probes/{id}", a.handleAppProbeResult)
	mux.HandleFunc("POST /api/app/clusters/{id}/remediation", a.handleAppEnableRemediation)
	mux.HandleFunc("POST /api/app/clusters/{id}/rightsize", a.handleAppRightsize)
	mux.HandleFunc("POST /api/app/clusters/{id}/chat", a.handleAppClusterChat)
	mux.HandleFunc("GET /api/app/clusters/{id}/chat", a.handleAppChatHistory)
	mux.HandleFunc("DELETE /api/app/clusters/{id}/chat", a.handleAppChatHistory)
	mux.HandleFunc("GET /api/app/chat/{job}", a.handleAppChatResult)
	mux.HandleFunc("POST /api/app/clusters/{id}/automation/{action}", a.handleAppClusterAutomation)
	mux.HandleFunc("POST /api/app/incidents/{id}/{action}", a.handleAppIncidentAction)
	mux.HandleFunc("POST /api/app/proposals/{id}/{action}", a.handleAppProposalAction)

	mux.HandleFunc("GET /remediation.yaml", handleRemediationManifest)
	mux.HandleFunc("GET /install-host.sh", handleHostInstallScript)
	mux.HandleFunc("/", a.pageHome)

	log.Printf("clusterheart account server on http://%s", *addr)
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
