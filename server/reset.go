package main

// Password reset. A stateless, single-use token is emailed as a link. The token
// embeds a fingerprint of the current password hash, so once the password is
// changed the token no longer validates - no server-side token storage needed.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/smtp"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const resetTTL = time.Hour

// publicBaseURL is the externally reachable base (behind Vercel/Cloudflare),
// used to build links we email out. Override with PUBLIC_BASE_URL.
func publicBaseURL() string {
	if v := os.Getenv("PUBLIC_BASE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://www.clusterheart.com"
}

func resetFingerprint(passHash string) string {
	h := sha256.Sum256([]byte(passHash))
	return hex.EncodeToString(h[:8])
}

// mintReset builds email|exp|fingerprint signed with the server secret.
func (a *app) mintReset(email, passHash string) string {
	msg := strings.ToLower(email) + "|" +
		time.Now().Add(resetTTL).UTC().Format(time.RFC3339) + "|" +
		resetFingerprint(passHash)
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte("reset:" + msg))
	return base64.RawURLEncoding.EncodeToString([]byte(msg)) + "." + hex.EncodeToString(mac.Sum(nil))
}

func (a *app) parseReset(token string) (email, fingerprint string, ok bool) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", "", false
	}
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte("reset:" + string(raw)))
	if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(parts[1])) {
		return "", "", false
	}
	f := strings.Split(string(raw), "|")
	if len(f) != 3 {
		return "", "", false
	}
	exp, err := time.Parse(time.RFC3339, f[1])
	if err != nil || time.Now().After(exp) {
		return "", "", false
	}
	return f[0], f[2], true
}

func (a *app) pageForgot(w http.ResponseWriter, r *http.Request) { render(w, forgotTmpl, nil) }
func (a *app) pageReset(w http.ResponseWriter, r *http.Request)  { render(w, resetTmpl, nil) }

// handleForgot always replies ok so it never reveals whether an email exists.
func (a *app) handleForgot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if strings.Contains(email, "@") {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if u, _ := a.store.find(ctx, email); u != nil {
			link := publicBaseURL() + "/reset?token=" + a.mintReset(u.Email, u.PassHash)
			go a.sendResetEmail(u.Email, u.Name, link)
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *app) handleReset(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	if len(body.Password) < 8 {
		writeJSON(w, 400, map[string]any{"error": "Password must be at least 8 characters."})
		return
	}
	email, fp, ok := a.parseReset(body.Token)
	if !ok {
		writeJSON(w, 400, map[string]any{"error": "This reset link is invalid or has expired."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	u, err := a.store.find(ctx, email)
	if err != nil || u == nil {
		writeJSON(w, 400, map[string]any{"error": "This reset link is invalid or has expired."})
		return
	}
	if resetFingerprint(u.PassHash) != fp {
		writeJSON(w, 400, map[string]any{"error": "This reset link has already been used."})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": "could not reset password"})
		return
	}
	if err := a.store.setPassword(ctx, email, string(hash)); err != nil {
		writeJSON(w, 500, map[string]any{"error": "could not reset password"})
		return
	}
	_ = a.store.invalidateSessions(ctx, email) // kill existing sessions after a reset
	writeJSON(w, 200, map[string]any{"ok": true})
}

// sendMail sends one HTML email over SMTP (Gmail), wrapped in the shared
// Clusterheart layout. Without SMTP settings it logs instead, so local dev works.
func sendMail(to, subject, inner string) error {
	host := envOr("SMTP_HOST", "smtp.gmail.com")
	port := envOr("SMTP_PORT", "587")
	user := os.Getenv("SMTP_USER")
	pass := os.Getenv("SMTP_PASS")
	from := envOr("SMTP_FROM", user)
	if user == "" || pass == "" {
		log.Printf("mail: SMTP not configured - would send %q to %s", subject, to)
		return nil
	}
	htmlBody := `<!DOCTYPE html><html><body style="font-family:-apple-system,Segoe UI,Roboto,sans-serif;background:#fff;color:#171717;padding:24px">` +
		inner + `<p style="color:#a3a3a3;font-size:12px;margin-top:26px">Clusterheart</p></body></html>`
	msg := "From: Clusterheart <" + from + ">\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + strings.NewReplacer("\r", "", "\n", "").Replace(subject) + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/html; charset=UTF-8\r\n\r\n" + htmlBody
	return smtp.SendMail(host+":"+port, smtp.PlainAuth("", user, pass, host), from, []string{to}, []byte(msg))
}

// sendResetEmail delivers the reset link via SMTP (Gmail). If SMTP isn't
// configured it logs the link instead so local/dev still works.
func (a *app) sendResetEmail(to, name, link string) {
	host := envOr("SMTP_HOST", "smtp.gmail.com")
	port := envOr("SMTP_PORT", "587")
	user := os.Getenv("SMTP_USER")
	pass := os.Getenv("SMTP_PASS")
	from := envOr("SMTP_FROM", user)
	if user == "" || pass == "" {
		log.Printf("reset: SMTP not configured - link for %s: %s", to, link)
		return
	}
	who := name
	if who == "" {
		who = "there"
	}
	htmlBody := fmt.Sprintf(`<!DOCTYPE html><html><body style="font-family:-apple-system,Segoe UI,Roboto,sans-serif;background:#fff;color:#171717;padding:24px">
<p>Hi %s,</p>
<p>We received a request to reset your Clusterheart password. Click the button below to choose a new one. This link expires in 1 hour and can be used once.</p>
<p style="margin:26px 0"><a href="%s" style="background:#000;color:#fff;text-decoration:none;font-weight:600;padding:12px 22px;display:inline-block">Reset password</a></p>
<p style="color:#525252;font-size:13px">Or paste this link into your browser:<br>%s</p>
<p style="color:#525252;font-size:13px">If you didn't request this, you can safely ignore this email - your password won't change.</p>
<p style="color:#a3a3a3;font-size:12px;margin-top:26px">Clusterheart</p>
</body></html>`, htmlEscape(who), link, link)

	msg := "From: Clusterheart <" + from + ">\r\n" +
		"To: " + to + "\r\n" +
		"Subject: Reset your Clusterheart password\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/html; charset=UTF-8\r\n\r\n" + htmlBody

	auth := smtp.PlainAuth("", user, pass, host)
	if err := smtp.SendMail(host+":"+port, auth, from, []string{to}, []byte(msg)); err != nil {
		log.Printf("reset: send to %s failed: %v", to, err)
		return
	}
	log.Printf("reset: email sent to %s", to)
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}
