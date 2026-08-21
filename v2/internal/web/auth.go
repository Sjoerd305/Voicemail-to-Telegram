package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/config"
)

const (
	sessionCookie = "vm_session"
	stateCookie   = "vm_oauth_state"
	sessionTTL    = 30 * 24 * time.Hour
)

// googleAuth protects the whole UI behind a Google sign-in. Sessions are
// HMAC-signed cookies; the signing key is derived from the client secret so
// sessions survive restarts without extra configuration.
type googleAuth struct {
	cfg    config.GoogleAuth
	public string // web.public_url, no trailing slash
	key    []byte
	secure bool
}

func newGoogleAuth(webCfg config.Web) *googleAuth {
	sum := sha256.Sum256([]byte("session-key:" + webCfg.GoogleAuth.ClientSecret))
	return &googleAuth{
		cfg:    webCfg.GoogleAuth,
		public: strings.TrimRight(webCfg.PublicURL, "/"),
		key:    sum[:],
		secure: strings.HasPrefix(webCfg.PublicURL, "https://"),
	}
}

func (a *googleAuth) oauthConfig() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     a.cfg.ClientID,
		ClientSecret: a.cfg.ClientSecret,
		RedirectURL:  a.public + "/auth/callback",
		Scopes:       []string{"openid", "email"},
		Endpoint:     google.Endpoint,
	}
}

func (a *googleAuth) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/login":
			a.handleLogin(w, r)
			return
		case "/auth/callback":
			a.handleCallback(w, r)
			return
		case "/auth/logout":
			a.clearSession(w)
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		if _, ok := a.session(r); ok {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		a.serveLoginPage(w, r)
	})
}

func (a *googleAuth) handleLogin(w http.ResponseWriter, r *http.Request) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	state := hex.EncodeToString(buf)
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state, Path: "/",
		MaxAge: 600, HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, a.oauthConfig().AuthCodeURL(state), http.StatusFound)
}

func (a *googleAuth) handleCallback(w http.ResponseWriter, r *http.Request) {
	fail := func(reason string) {
		slog.Warn("google login rejected", "reason", reason)
		http.Redirect(w, r, "/?e="+reason, http.StatusFound)
	}
	stateC, err := r.Cookie(stateCookie)
	if err != nil || stateC.Value == "" || r.URL.Query().Get("state") != stateC.Value {
		fail("state")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		fail("failed")
		return
	}
	tok, err := a.oauthConfig().Exchange(r.Context(), code)
	if err != nil {
		slog.Error("oauth exchange failed", "err", err)
		fail("failed")
		return
	}
	idToken, _ := tok.Extra("id_token").(string)
	email, verified, err := parseIDToken(idToken)
	if err != nil || !verified {
		fail("failed")
		return
	}
	if !a.emailAllowed(email) {
		slog.Warn("login from disallowed account", "email", email)
		fail("domain")
		return
	}
	a.setSession(w, email)
	slog.Info("user logged in", "email", email)
	http.Redirect(w, r, "/", http.StatusFound)
}

// parseIDToken extracts the claims we need from a Google id_token. The token
// comes straight from Google's token endpoint over TLS, so decoding without
// signature verification is safe here.
func parseIDToken(idToken string) (email string, verified bool, err error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return "", false, fmt.Errorf("malformed id_token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false, err
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", false, err
	}
	return strings.ToLower(claims.Email), claims.EmailVerified, nil
}

// emailAllowed checks the account against allowed_domains. Entries may be a
// domain ("bedrijf.nl") or, when they contain an @, one specific address
// ("extern@gmail.com").
func (a *googleAuth) emailAllowed(email string) bool {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return false
	}
	domain := email[at+1:]
	for _, entry := range a.cfg.AllowedDomains {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "@") {
			if entry == email {
				return true
			}
		} else if entry == domain {
			return true
		}
	}
	return false
}

// --- session cookie ---------------------------------------------------------

func (a *googleAuth) sign(payload string) string {
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *googleAuth) setSession(w http.ResponseWriter, email string) {
	exp := time.Now().Add(sessionTTL)
	value := a.sign(email + "|" + strconv.FormatInt(exp.Unix(), 10))
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: value, Path: "/",
		Expires: exp, HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode,
	})
}

func (a *googleAuth) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/",
		MaxAge: -1, HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode,
	})
}

// session validates the cookie and returns the logged-in email.
func (a *googleAuth) session(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	dot := strings.LastIndex(c.Value, ".")
	if dot < 0 {
		return "", false
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(c.Value[:dot])
	if err != nil {
		return "", false
	}
	if a.sign(string(payloadRaw)) != c.Value {
		return "", false
	}
	payload := string(payloadRaw)
	sep := strings.LastIndex(payload, "|")
	if sep < 0 {
		return "", false
	}
	expUnix, err := strconv.ParseInt(payload[sep+1:], 10, 64)
	if err != nil || time.Now().Unix() > expUnix {
		return "", false
	}
	email := payload[:sep]
	if !a.emailAllowed(email) {
		// Domain list changed since the cookie was issued.
		return "", false
	}
	return email, true
}

// --- login page -------------------------------------------------------------

var loginTmpl = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="nl">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Voicemail – Inloggen</title>
<style>
:root { --bg:#f5f6f8; --card:#fff; --text:#1a1d21; --muted:#6b7280; --border:#e2e5ea; --accent:#2563eb; --err:#dc2626; }
@media (prefers-color-scheme: dark) {
  :root { --bg:#101318; --card:#191e26; --text:#e8eaed; --muted:#8b94a1; --border:#2a313b; --accent:#4d94f8; --err:#f87171; }
}
* { box-sizing:border-box; }
body { margin:0; min-height:100vh; display:grid; place-items:center;
  font:15px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif; background:var(--bg); color:var(--text); }
.card { background:var(--card); border:1px solid var(--border); border-radius:14px;
  padding:2.2rem 2.4rem; width:min(90vw,360px); text-align:center; box-shadow:0 1px 3px rgba(16,24,40,.07); }
.brand { display:flex; align-items:center; justify-content:center; gap:.55rem;
  font-size:1.15rem; font-weight:650; margin-bottom:.4rem; }
.brand svg { color:var(--accent); }
p { color:var(--muted); font-size:.88rem; margin:0 0 1.5rem; }
.gbtn { display:flex; align-items:center; justify-content:center; gap:.65rem; width:100%;
  padding:.7rem 1rem; border:1px solid var(--border); border-radius:10px; background:var(--card);
  color:var(--text); font-size:.92rem; font-weight:550; text-decoration:none; }
.gbtn:hover { border-color:var(--accent); }
.error { color:var(--err); font-size:.82rem; margin-top:1rem; }
</style>
</head>
<body>
<main class="card">
  <div class="brand">
    <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M6 10a3 3 0 1 0 0 6 3 3 0 0 0 0-6Z"/><path d="M18 10a3 3 0 1 0 0 6 3 3 0 0 0 0-6Z"/><path d="M6 16h12"/></svg>
    Voicemail
  </div>
  <p>Log in met je Google-account om verder te gaan.</p>
  <a class="gbtn" href="/auth/login">
    <svg width="18" height="18" viewBox="0 0 48 48"><path fill="#EA4335" d="M24 9.5c3.54 0 6.71 1.22 9.21 3.6l6.85-6.85C35.9 2.38 30.47 0 24 0 14.62 0 6.51 5.38 2.56 13.22l7.98 6.19C12.43 13.72 17.74 9.5 24 9.5z"/><path fill="#4285F4" d="M46.98 24.55c0-1.57-.15-3.09-.38-4.55H24v9.02h12.94c-.58 2.96-2.26 5.48-4.78 7.18l7.73 6c4.51-4.18 7.09-10.36 7.09-17.65z"/><path fill="#FBBC05" d="M10.53 28.59c-.48-1.45-.76-2.99-.76-4.59s.27-3.14.76-4.59l-7.98-6.19C.92 16.46 0 20.12 0 24c0 3.88.92 7.54 2.56 10.78l7.97-6.19z"/><path fill="#34A853" d="M24 48c6.48 0 11.93-2.13 15.89-5.81l-7.73-6c-2.15 1.45-4.92 2.3-8.16 2.3-6.26 0-11.57-4.22-13.47-9.91l-7.98 6.19C6.51 42.62 14.62 48 24 48z"/></svg>
    Inloggen met Google
  </a>
  {{if eq .Error "domain"}}<div class="error">Dit account heeft geen toegang.</div>{{end}}
  {{if eq .Error "failed"}}<div class="error">Inloggen mislukt, probeer het opnieuw.</div>{{end}}
  {{if eq .Error "state"}}<div class="error">Sessie verlopen, probeer het opnieuw.</div>{{end}}
</main>
</body>
</html>`))

func (a *googleAuth) serveLoginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	loginTmpl.Execute(w, struct{ Error string }{Error: r.URL.Query().Get("e")})
}
