package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/actions"
	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/config"
	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/mailer"
	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/store"
)

func newTestServer(t *testing.T, password string) *httptest.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SaveVoicemail(&store.Voicemail{
		ReceivedAt:    time.Now(),
		Subject:       "PBX test",
		Transcription: "hallo dit is een test",
		MessageID:     "<test@example.com>",
	}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Web: config.Web{Password: password}}
	srv := NewServer(cfg, st, actions.New(cfg), mailer.NewWatcher(cfg, st, nil, nil), nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

type fakeNotifier struct {
	mu       sync.Mutex
	messages []string
}

func (f *fakeNotifier) SendMessage(text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, text)
	return nil
}

func TestActionAnnouncesInTelegram(t *testing.T) {
	// Backend the http-type command calls.
	pbx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(pbx.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{Commands: map[string]config.Command{
		"testcmd": {Type: "http", URL: pbx.URL, Success: "Gelukt."},
	}}
	notifier := &fakeNotifier{}
	srv := NewServer(cfg, st, actions.New(cfg), mailer.NewWatcher(cfg, st, nil, nil), notifier)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/api/actions/testcmd", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("action status: %d", resp.StatusCode)
	}

	// The announcement is sent asynchronously.
	deadline := time.Now().Add(2 * time.Second)
	for {
		notifier.mu.Lock()
		msgs := append([]string(nil), notifier.messages...)
		notifier.mu.Unlock()
		if len(msgs) > 0 {
			if msgs[0] != "Gelukt. (via web-UI)" {
				t.Fatalf("unexpected announcement: %q", msgs[0])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no telegram announcement sent")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAPIEndpoints(t *testing.T) {
	ts := newTestServer(t, "")

	resp, err := http.Get(ts.URL + "/api/voicemails")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("voicemails status: %d", resp.StatusCode)
	}
	var page store.Page
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Total != 1 || page.HasMore || page.Items[0].Transcription != "hallo dit is een test" {
		t.Fatalf("unexpected voicemails page: %+v", page)
	}

	for _, path := range []string{"/api/status", "/api/stats", "/api/events", "/api/actions", "/"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s status: %d", path, resp.StatusCode)
		}
	}
}

func TestSetDone(t *testing.T) {
	ts := newTestServer(t, "")

	mark := func(done bool) store.Voicemail {
		t.Helper()
		resp, err := http.Post(ts.URL+"/api/voicemails/1/done", "application/json",
			strings.NewReader(fmt.Sprintf(`{"done":%v}`, done)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("done status: %d", resp.StatusCode)
		}
		var vm store.Voicemail
		if err := json.NewDecoder(resp.Body).Decode(&vm); err != nil {
			t.Fatal(err)
		}
		return vm
	}

	if vm := mark(true); !vm.Done || vm.DoneAt.IsZero() {
		t.Fatalf("expected done with timestamp, got %+v", vm)
	}
	if vm := mark(false); vm.Done || !vm.DoneAt.IsZero() {
		t.Fatalf("expected reopened, got %+v", vm)
	}

	resp, err := http.Post(ts.URL+"/api/voicemails/999/done", "application/json",
		strings.NewReader(`{"done":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown id, got %d", resp.StatusCode)
	}
}

func TestBasicAuth(t *testing.T) {
	ts := newTestServer(t, "geheim")

	resp, err := http.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without password, got %d", resp.StatusCode)
	}

	req, _ := http.NewRequest("GET", ts.URL+"/api/status", nil)
	req.SetBasicAuth("admin", "geheim")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 with password, got %d", resp.StatusCode)
	}
}

// A web action must be credited to the signed-in Google account, both in the
// event log and in the Telegram announcement.
func TestActionLogsSignedInUser(t *testing.T) {
	pbx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(pbx.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	notifier := &fakeNotifier{}
	ts, post := newSignedInTestServer(t, st, map[string]config.Command{
		"testcmd": {Type: "http", URL: pbx.URL, Success: "Gelukt."},
	}, notifier)

	resp := post(t, ts.URL+"/api/actions/testcmd", "")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("action status: %d", resp.StatusCode)
	}

	events, err := st.ListEvents(10)
	if err != nil {
		t.Fatal(err)
	}
	want := "/testcmd by Sjoerd van Dijk (web): Gelukt."
	if len(events) == 0 || events[0].Detail != want {
		t.Fatalf("event detail = %+v, want %q", events, want)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		notifier.mu.Lock()
		msgs := append([]string(nil), notifier.messages...)
		notifier.mu.Unlock()
		if len(msgs) > 0 {
			if msgs[0] != "Gelukt. (via web-UI door Sjoerd van Dijk)" {
				t.Fatalf("unexpected announcement: %q", msgs[0])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no telegram announcement sent")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// newSignedInTestServer builds a server behind Google auth and returns a post
// helper that carries a valid session cookie, so tests can check what gets
// attributed to the signed-in user.
func newSignedInTestServer(t *testing.T, st *store.Store, commands map[string]config.Command, notifier Notifier) (*httptest.Server, func(*testing.T, string, string) *http.Response) {
	t.Helper()
	webCfg := config.Web{
		PublicURL: "http://voicemail.local:8080",
		GoogleAuth: config.GoogleAuth{
			ClientID: "id", ClientSecret: "secret", AllowedDomains: []string{"smvd.net"},
		},
	}
	cfg := &config.Config{Web: webCfg, Commands: commands}
	srv := NewServer(cfg, st, actions.New(cfg), mailer.NewWatcher(cfg, st, nil, nil), notifier)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	sess := httptest.NewRecorder()
	newGoogleAuth(webCfg).setSession(sess, sessionUser{Email: "sjoerd@smvd.net", Name: "Sjoerd van Dijk"})
	post := func(t *testing.T, url, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest("POST", url, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range sess.Result().Cookies() {
			req.AddCookie(c)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			t.Fatalf("POST %s: status %d", url, resp.StatusCode)
		}
		return resp
	}
	return ts, post
}

// Marking a voicemail done is credited to the signed-in account too.
func TestSetDoneLogsSignedInUser(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	vm := &store.Voicemail{ReceivedAt: time.Now(), Subject: "PBX test", MessageID: "<done@example.com>"}
	if err := st.SaveVoicemail(vm); err != nil {
		t.Fatal(err)
	}

	ts, post := newSignedInTestServer(t, st, nil, nil)
	url := fmt.Sprintf("%s/api/voicemails/%d/done", ts.URL, vm.ID)

	for _, tc := range []struct{ body, want string }{
		{`{"done":true}`, fmt.Sprintf("voicemail #%d afgehandeld door Sjoerd van Dijk", vm.ID)},
		{`{"done":false}`, fmt.Sprintf("voicemail #%d heropend door Sjoerd van Dijk", vm.ID)},
	} {
		post(t, url, tc.body).Body.Close()
		events, err := st.ListEvents(10)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) == 0 || events[0].Detail != tc.want {
			t.Fatalf("event detail = %+v, want %q", events, tc.want)
		}
	}
}
