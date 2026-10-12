package xurrent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Sjoerd305/Voicemail-to-Telegram/internal/config"
	"github.com/Sjoerd305/Voicemail-to-Telegram/internal/store"
)

// fakeXurrent serves the person "Vivia" (7), team "Engineers" (5, members 8 and 9 over two pages), plus the given requests
// and notes, filtering them like the real API. All notes are on request
// #100, an older request that is updated whenever a note is added.
type fakeXurrent struct {
	notes      []Note
	requests   []Request
	reqTeam    map[int]int // request id -> team id
	fail       bool
	wantPeople string // expected person/created_by filter
	paths      []string
	queries    []string
}

func (f *fakeXurrent) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("X-Xurrent-Account") != "acme" {
			t.Errorf("missing auth headers: %v", r.Header)
		}
		q := r.URL.Query()
		f.paths = append(f.paths, r.URL.Path)
		f.queries = append(f.queries, r.URL.RawQuery)
		if f.fail && strings.HasPrefix(r.URL.Path, "/requests") {
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
			return
		}
		inRange := func(at time.Time) bool {
			a, b, _ := strings.Cut(strings.TrimPrefix(q.Get("created_at"), ">="), "<")
			from, _ := time.Parse(time.RFC3339, a)
			to, _ := time.Parse(time.RFC3339, b)
			return !at.Before(from) && at.Before(to)
		}
		var out any
		switch r.URL.Path {
		case "/people":
			// Name filtering is exact in Xurrent; return a near miss too to
			// check that we compare ourselves as well.
			// check that we compare ourselves as well, and a disabled old
			// account with the same name that must be ignored.
			out = []Ref{{ID: 7, Name: "Vivia"}, {ID: 70, Name: "Vivia Test"}, {ID: 71, Name: "Vivia", Disabled: true}}
		case "/teams":
			out = []Ref{{ID: map[string]int{"Engineers": 5}[q.Get("name")], Name: q.Get("name")}}
		case "/teams/5/members":
			if q.Get("page") == "" {
				w.Header().Set("Link", `<http://`+r.Host+`/teams/5/members?page=2>; rel="next"`)
				out = []Ref{{ID: 8, Name: "Piet"}}
			} else {
				out = []Ref{{ID: 9, Name: "Klaas"}}
			}
		case "/requests/100/notes":
			if f.wantPeople != "" && q.Get("person") != f.wantPeople {
				t.Errorf("person filter = %q, want %q", q.Get("person"), f.wantPeople)
			}
			if q.Get("medium") != "default" {
				t.Errorf("medium filter = %q", q.Get("medium"))
			}
			notes := []Note{}
			for _, n := range f.notes {
				if inRange(n.CreatedAt) {
					notes = append(notes, n)
				}
			}
			out = notes
		case "/requests":
			reqs := []Request{}
			if since := q.Get("updated_at"); since != "" {
				from, _ := time.Parse(time.RFC3339, strings.TrimPrefix(since, ">="))
				holder := Request{ID: 100, CreatedAt: t0.Add(-24 * time.Hour), UpdatedAt: t0.Add(-24 * time.Hour)}
				for _, n := range f.notes {
					if n.CreatedAt.After(holder.UpdatedAt) {
						holder.UpdatedAt = n.CreatedAt
					}
				}
				for _, rq := range append([]Request{holder}, f.requests...) {
					if !rq.UpdatedAt.Before(from) {
						reqs = append(reqs, rq)
					}
				}
				out = reqs
				break
			}
			for _, rq := range f.requests {
				if team := q.Get("team"); team != "" && team != strconv.Itoa(f.reqTeam[rq.ID]) {
					continue
				}
				if inRange(rq.CreatedAt) {
					reqs = append(reqs, rq)
				}
			}
			out = reqs
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	})
}

func newTestChecker(t *testing.T, f *fakeXurrent) (*Checker, config.Xurrent) {
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	cfg := config.Xurrent{
		Enabled: true, BaseURL: srv.URL, Account: "acme", Token: "tok",
		People: []string{"Vivia"},
		Window: time.Hour, RepeatWindow: 4 * time.Hour,
	}
	return NewChecker(cfg), cfg
}

var t0 = time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)

func TestCheckFindsNoteWithinWindow(t *testing.T) {
	f := &fakeXurrent{wantPeople: "7", notes: []Note{
		{ID: 1, Person: Ref{ID: 7, Name: "Vivia"}, CreatedAt: t0.Add(-5 * time.Minute), Text: "voor de voicemail"},
		{ID: 2, Person: Ref{ID: 7, Name: "Vivia"}, CreatedAt: t0.Add(20 * time.Minute), Text: "Klant teruggebeld"},
	}}
	c, _ := newTestChecker(t, f)
	v, err := c.Check(context.Background(), t0, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != StatusActivity || v.Activity.Text != "Klant teruggebeld" || v.Activity.Person != "Vivia" {
		t.Fatalf("got %+v", v.Activity)
	}
}

func TestCheckRequestOnAnyTeamCounts(t *testing.T) {
	f := &fakeXurrent{
		requests: []Request{{ID: 4521, Subject: "Lift defect", CreatedAt: t0.Add(10 * time.Minute)}},
		reqTeam:  map[int]int{4521: 99}, // moved to another team since
	}
	c, _ := newTestChecker(t, f)
	v, err := c.Check(context.Background(), t0, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != StatusActivity || v.Activity.RequestID != 4521 {
		t.Fatalf("got %+v", v.Activity)
	}
	for _, q := range f.queries {
		if strings.Contains(q, "team=") {
			t.Fatalf("request listing filtered on team: %s", q)
		}
	}
}

func TestPersonIDsNeedNoLookup(t *testing.T) {
	f := &fakeXurrent{
		requests: []Request{{ID: 4521, CreatedAt: t0.Add(10 * time.Minute)}},
	}
	_, cfg := newTestChecker(t, f)
	cfg.People, cfg.PersonIDs = nil, []int{7}
	v, err := NewChecker(cfg).Check(context.Background(), t0, t0.Add(2*time.Hour))
	if err != nil || v.Status != StatusActivity {
		t.Fatalf("got %+v, %v", v, err)
	}
	for _, q := range f.paths {
		if q == "/people" || q == "/teams" {
			t.Fatalf("looked up %s although ids were given", q)
		}
	}
}

func TestMemberTeamsAddPeople(t *testing.T) {
	f := &fakeXurrent{wantPeople: "7,8,9"}
	c, cfg := newTestChecker(t, f)
	cfg.MemberTeams = []string{"Engineers"}
	c = NewChecker(cfg)
	people, err := c.People(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(people) != 3 || people[7] != "Vivia" || people[9] != "Klaas (Engineers)" {
		t.Fatalf("people = %v", people)
	}
	if _, err := c.Check(context.Background(), t0, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func TestCheckPendingThenNone(t *testing.T) {
	f := &fakeXurrent{notes: []Note{{ID: 1, Person: Ref{ID: 7}, CreatedAt: t0.Add(90 * time.Minute)}}}
	c, _ := newTestChecker(t, f)
	v, err := c.Check(context.Background(), t0, t0.Add(30*time.Minute))
	if err != nil || v.Status != StatusPending {
		t.Fatalf("inside window: %+v %v", v, err)
	}
	// The note after the window must not count.
	v, err = c.Check(context.Background(), t0, t0.Add(2*time.Hour))
	if err != nil || v.Status != StatusNone {
		t.Fatalf("after window: %+v %v", v, err)
	}
}

func TestCheckErrorIsNotNone(t *testing.T) {
	c, _ := newTestChecker(t, &fakeXurrent{fail: true})
	if _, err := c.Check(context.Background(), t0, t0.Add(2*time.Hour)); err == nil {
		t.Fatal("expected an error, a failing API must never read as no activity")
	}
}

func TestCallerNumber(t *testing.T) {
	for in, want := range map[string]string{
		"Klantnaam: X\nFrom:    \"0657553610\" <0657553610>": "0657553610",
		"From: \"Bakkerij\" <+31 6 5755 3610>":               "0657553610",
		"From: \"+31657553610\" <+31657553610>":              "0657553610",
		"From: \"anonymous\" <anonymous>":                    "",
		"geen afzender":                                      "",
	} {
		if got := CallerNumber(in); got != want {
			t.Errorf("CallerNumber(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepeatOf(t *testing.T) {
	earlier := []Earlier{
		{ID: 1, ReceivedAt: t0.Add(-5 * time.Hour), Caller: "0612345678", Status: StatusActivity}, // too old
		{ID: 2, ReceivedAt: t0.Add(-2 * time.Hour), Caller: "0612345678", Status: StatusActivity},
		{ID: 3, ReceivedAt: t0.Add(-time.Hour), Caller: "0612345678", Status: StatusNone},
		{ID: 4, ReceivedAt: t0.Add(-time.Hour), Caller: "0699999999", Status: StatusActivity},
	}
	if e := RepeatOf("0612345678", t0, 4*time.Hour, earlier); e == nil || e.ID != 2 {
		t.Fatalf("got %+v, want #2", e)
	}
	if e := RepeatOf("0611111111", t0, 4*time.Hour, earlier); e != nil {
		t.Fatalf("unrelated caller matched %+v", e)
	}
	if e := RepeatOf("", t0, 4*time.Hour, earlier); e != nil {
		t.Fatal("anonymous caller must never match")
	}
}

type fakeNotifier struct{ sent []string }

func (f *fakeNotifier) SendMessage(text string) error { f.sent = append(f.sent, text); return nil }

func TestMonitorReportsNoneAndRepeatCaller(t *testing.T) {
	f := &fakeXurrent{notes: []Note{{ID: 1, Person: Ref{ID: 7, Name: "Vivia"}, CreatedAt: t0.Add(15 * time.Minute)}}}
	_, cfg := newTestChecker(t, f)
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	n := &fakeNotifier{}
	m := NewMonitor(cfg, st, n)

	caller := "Klantnaam: Bakkerij\nFrom: \"0612345678\" <0612345678>"
	first := &store.Voicemail{ReceivedAt: t0, EmailText: caller, MessageID: "a", XurrentStatus: StatusPending}
	// Same caller 2h later, no new activity: covered by the first one.
	repeat := &store.Voicemail{ReceivedAt: t0.Add(2 * time.Hour), EmailText: caller, MessageID: "b", XurrentStatus: StatusPending}
	// Other caller, no activity at all.
	missed := &store.Voicemail{ReceivedAt: t0.Add(2 * time.Hour), EmailText: "From: \"0699999999\" <0699999999>", MessageID: "c", XurrentStatus: StatusPending}
	for _, vm := range []*store.Voicemail{first, repeat, missed} {
		if err := st.SaveVoicemail(vm); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := st.XurrentPending()
	if err != nil || len(pending) != 3 {
		t.Fatalf("pending = %d, %v", len(pending), err)
	}
	m.sweepAt(context.Background(), t0.Add(4*time.Hour))

	want := map[int64]string{first.ID: StatusActivity, repeat.ID: StatusActivity, missed.ID: StatusNone}
	for id, status := range want {
		got, err := st.GetVoicemail(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.XurrentStatus != status {
			t.Errorf("#%d status = %q (%s), want %q", id, got.XurrentStatus, got.XurrentDetail, status)
		}
	}
	if len(n.sent) != 1 || !strings.Contains(n.sent[0], "0699999999") {
		t.Fatalf("telegram messages = %q", n.sent)
	}
	if p, _ := st.XurrentPending(); len(p) != 0 {
		t.Fatalf("still pending: %d", len(p))
	}
}

func TestMonitorKeepsPendingOnError(t *testing.T) {
	_, cfg := newTestChecker(t, &fakeXurrent{fail: true})
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	n := &fakeNotifier{}
	m := NewMonitor(cfg, st, n)
	vm := &store.Voicemail{ReceivedAt: t0, MessageID: "a", XurrentStatus: StatusPending}
	if err := st.SaveVoicemail(vm); err != nil {
		t.Fatal(err)
	}
	m.sweepAt(context.Background(), t0.Add(2*time.Hour))
	if got, _ := st.GetVoicemail(vm.ID); got.XurrentStatus != StatusPending || len(n.sent) != 0 {
		t.Fatalf("an API failure must leave the check pending, got %q, sent %q", got.XurrentStatus, n.sent)
	}
	// After a day of failures it gives up as error, never as none.
	m.sweepAt(context.Background(), t0.Add(26*time.Hour))
	if got, _ := st.GetVoicemail(vm.ID); got.XurrentStatus != StatusError {
		t.Fatalf("status = %q, want error", got.XurrentStatus)
	}
}

func TestClientRetriesAfterRateLimit(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"message":"Too Many Requests"}`, http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`[{"id":3,"name":"Engineers"}]`))
	}))
	defer srv.Close()
	team, err := NewClient(srv.URL, "acme", "tok").FindTeam(context.Background(), "Engineers")
	if err != nil || team.ID != 3 || calls != 2 {
		t.Fatalf("team %+v, err %v, calls %d", team, err, calls)
	}
}
