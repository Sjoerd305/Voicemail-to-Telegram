package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/config"
)

func testAuth() *googleAuth {
	return newGoogleAuth(config.Web{
		PublicURL: "http://voicemail.local:8080",
		GoogleAuth: config.GoogleAuth{
			ClientID:       "id",
			ClientSecret:   "secret",
			AllowedDomains: []string{"smvd.net", "extern@gmail.com"},
		},
	})
}

func TestEmailAllowed(t *testing.T) {
	a := testAuth()
	cases := map[string]bool{
		"sjoerd@smvd.net":     true,
		"iemand@smvd.net":     true,
		"extern@gmail.com":    true,  // explicitly allowed address
		"ander@gmail.com":     false, // same domain, not listed
		"sjoerd@evil.com":     false,
		"smvd.net":            false, // no @
		"sjoerd@sub.smvd.net": false, // subdomains are not implied
	}
	for email, want := range cases {
		if got := a.emailAllowed(email); got != want {
			t.Errorf("emailAllowed(%q) = %v, want %v", email, got, want)
		}
	}
}

func TestSessionRoundTrip(t *testing.T) {
	a := testAuth()
	rec := httptest.NewRecorder()
	a.setSession(rec, sessionUser{Email: "sjoerd@smvd.net", Name: "Sjoerd van Dijk"})

	req := httptest.NewRequest("GET", "/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	user, ok := a.session(req)
	if !ok || user.Email != "sjoerd@smvd.net" || user.Name != "Sjoerd van Dijk" {
		t.Fatalf("session not accepted: %+v %v", user, ok)
	}

	// Tampered cookie must be rejected.
	req2 := httptest.NewRequest("GET", "/", nil)
	c := rec.Result().Cookies()[0]
	req2.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value + "x"})
	if _, ok := a.session(req2); ok {
		t.Fatal("tampered session accepted")
	}

	// A session for a since-removed domain must be rejected.
	a2 := testAuth()
	a2.cfg.AllowedDomains = []string{"ander.nl"}
	if _, ok := a2.session(req); ok {
		t.Fatal("session for removed domain accepted")
	}
}

func TestMiddlewareBlocksAnonymous(t *testing.T) {
	a := testAuth()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("app"))
	})
	h := a.middleware(next)

	// Page request -> login screen.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	body, _ := io.ReadAll(rec.Result().Body)
	if !strings.Contains(string(body), "Inloggen met Google") {
		t.Fatalf("expected login page, got: %.100s", body)
	}

	// API request -> 401 JSON.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/voicemails", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for api, got %d", rec.Code)
	}

	// With a valid session everything passes.
	sess := httptest.NewRecorder()
	a.setSession(sess, sessionUser{Email: "sjoerd@smvd.net", Name: "Sjoerd van Dijk"})
	req := httptest.NewRequest("GET", "/api/voicemails", nil)
	for _, c := range sess.Result().Cookies() {
		req.AddCookie(c)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "app" {
		t.Fatalf("valid session blocked: %d %q", rec.Code, rec.Body.String())
	}
}

func TestLoginRedirectsToGoogle(t *testing.T) {
	a := testAuth()
	rec := httptest.NewRecorder()
	a.middleware(nil).ServeHTTP(rec, httptest.NewRequest("GET", "/auth/login", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("expected redirect, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://accounts.google.com/") ||
		!strings.Contains(loc, "voicemail.local%3A8080%2Fauth%2Fcallback") {
		t.Fatalf("unexpected redirect target: %s", loc)
	}
	if len(rec.Result().Cookies()) == 0 {
		t.Fatal("no state cookie set")
	}
}

func TestDisplayName(t *testing.T) {
	cases := []struct {
		user sessionUser
		want string
	}{
		{sessionUser{Email: "sjoerd@smvd.net", Name: "Sjoerd van Dijk"}, "Sjoerd van Dijk"},
		// Sessions issued before the profile scope carry no name.
		{sessionUser{Email: "sjoerd.van.dijk@smvd.net"}, "Sjoerd Van Dijk"},
		{sessionUser{Email: "sjoerd@smvd.net"}, "Sjoerd"},
		{sessionUser{Email: "@smvd.net"}, "@smvd.net"},
	}
	for _, c := range cases {
		if got := c.user.DisplayName(); got != c.want {
			t.Errorf("DisplayName(%+v) = %q, want %q", c.user, got, c.want)
		}
	}
}

// Cookies minted before the name was part of the payload must keep working.
func TestSessionAcceptsLegacyCookie(t *testing.T) {
	a := testAuth()
	exp := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: a.sign("sjoerd@smvd.net|" + exp)})

	user, ok := a.session(req)
	if !ok || user.Email != "sjoerd@smvd.net" || user.Name != "" {
		t.Fatalf("legacy session not accepted: %+v %v", user, ok)
	}
	if got := user.DisplayName(); got != "Sjoerd" {
		t.Fatalf("legacy display name = %q", got)
	}
}
