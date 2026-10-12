package xurrent

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Sjoerd305/Voicemail-to-Telegram/internal/config"
)

// Check outcomes, stored per voicemail.
const (
	StatusPending  = "pending"  // window still open, nothing seen yet
	StatusActivity = "activity" // a tracked person acted in Xurrent within the window
	StatusNone     = "none"     // window closed without activity; Xurrent answered fine
	StatusError    = "error"    // Xurrent could not be checked; never treated as "none"
)

// Activity is one thing a tracked person did in Xurrent.
type Activity struct {
	At        time.Time
	Kind      string // "request" or "note"
	Person    string // "Jan Jansen (Vivia)"
	RequestID int
	Text      string // request subject or start of the note
}

func (a Activity) Describe() string {
	what := fmt.Sprintf("notitie op #%d", a.RequestID)
	if a.Kind == "request" {
		what = fmt.Sprintf("nieuwe melding #%d", a.RequestID)
	}
	s := fmt.Sprintf("%s door %s om %s", what, a.Person, a.At.In(time.Local).Format("15:04"))
	if a.Text != "" {
		s += ": " + a.Text
	}
	return s
}

// Checker finds activity of the configured teams and people.
type Checker struct {
	client *Client
	cfg    config.Xurrent

	// Progress, when set, is called while notes are fetched.
	Progress func(done, total int)

	mu         sync.Mutex
	people     map[int]string // person id -> label
	resolvedAt time.Time
}

func NewChecker(cfg config.Xurrent) *Checker {
	return &Checker{client: NewClient(cfg.BaseURL, cfg.Account, cfg.Token), cfg: cfg}
}

// peopleTTL: names and team membership are looked up again after this
// long, so a change in Xurrent is picked up without a restart.
const peopleTTL = 6 * time.Hour

// People resolves the configured names and teams to Xurrent person ids.
// Team membership is the current one, also when checking old voicemails.
func (c *Checker) People(ctx context.Context) (map[int]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.people != nil && time.Since(c.resolvedAt) < peopleTTL {
		return c.people, nil
	}
	people := map[int]string{}
	for _, id := range c.cfg.PersonIDs {
		people[id] = fmt.Sprintf("persoon %d", id)
	}
	for _, name := range c.cfg.People {
		p, err := c.client.FindPerson(ctx, name)
		if err != nil {
			return nil, err
		}
		people[p.ID] = p.Name
	}
	for _, name := range c.cfg.MemberTeams {
		team, err := c.client.FindTeam(ctx, name)
		if err != nil {
			return nil, err
		}
		members, err := c.client.TeamMembers(ctx, team.ID)
		if err != nil {
			return nil, fmt.Errorf("members of %s: %w", name, err)
		}
		for _, m := range members {
			if _, named := people[m.ID]; !named {
				people[m.ID] = fmt.Sprintf("%s (%s)", m.Name, team.Name)
			}
		}
	}
	if len(people) == 0 {
		// An empty list would make every check come back empty: "none"
		// for every voicemail. Refuse instead.
		return nil, fmt.Errorf("xurrent: no people to track (member_teams have no members)")
	}
	c.people = people
	c.resolvedAt = time.Now()
	return people, nil
}

// noteWorkers is how many requests' notes are fetched at the same time.
const noteWorkers = 4

// Activity lists what tracked people did in Xurrent in [from, to), oldest
// first: requests they created and notes they typed
// on any request. Notes are read per request, from every request updated
// since from, so a long range costs one call per updated request.
func (c *Checker) Activity(ctx context.Context, from, to time.Time) ([]Activity, error) {
	people, err := c.People(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]int, 0, len(people))
	for id := range people {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	created, err := c.client.RequestsCreatedBy(ctx, ids, from, to)
	if err != nil {
		return nil, err
	}
	updated, err := c.client.RequestsUpdatedSince(ctx, from)
	if err != nil {
		return nil, err
	}
	var candidates []Request
	for _, r := range updated {
		// A request created after the range cannot hold a note in it.
		if r.CreatedAt.Before(to) {
			candidates = append(candidates, r)
		}
	}

	var out []Activity
	for _, r := range created {
		who := c.Label()
		if r.CreatedBy != nil && r.CreatedBy.Name != "" {
			who = r.CreatedBy.Name
		}
		out = append(out, Activity{At: r.CreatedAt, Kind: "request", Person: or(who, "gevolgde persoon"), RequestID: r.ID, Text: r.Subject})
	}

	type result struct {
		req   Request
		notes []Note
		err   error
	}
	jobs := make(chan Request)
	results := make(chan result)
	for i := 0; i < noteWorkers; i++ {
		go func() {
			for r := range jobs {
				notes, err := c.client.RequestNotes(ctx, r.ID, ids, from, to)
				results <- result{r, notes, err}
			}
		}()
	}
	// Every candidate is sent and every result received, also after an
	// error, so no goroutine is left blocked. A cancelled ctx makes the
	// remaining calls fail immediately.
	go func() {
		defer close(jobs)
		for _, r := range candidates {
			jobs <- r
		}
	}()
	var firstErr error
	for done := 1; done <= len(candidates); done++ {
		res := <-results
		if c.Progress != nil {
			c.Progress(done, len(candidates))
		}
		if res.err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("notes of #%d: %w", res.req.ID, res.err)
			}
			continue
		}
		for _, n := range res.notes {
			label, ok := people[n.Person.ID]
			if !ok {
				// The filter should have excluded it; don't let a stray
				// note from someone else count.
				continue
			}
			if n.Person.Name != "" && c.isIDOnly(n.Person.ID) {
				label = n.Person.Name
			}
			out = append(out, Activity{At: n.CreatedAt, Kind: "note", Person: label, RequestID: res.req.ID, Text: snippet(n.Text, 80)})
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

func (c *Checker) isIDOnly(id int) bool {
	for _, x := range c.cfg.PersonIDs {
		if x == id {
			return true
		}
	}
	return false
}

// Label names whose activity is tracked, e.g. "Vivia", or "" when they are
// only configured by id. The request listing does not say which of them
// created a request, so requests get this label.
func (c *Checker) Label() string {
	return strings.Join(append(append([]string{}, c.cfg.People...), c.cfg.MemberTeams...), "/")
}

// Verdict is the outcome of checking one voicemail at a point in time.
type Verdict struct {
	Status   string
	Activity *Activity // first activity, for StatusActivity
	Detail   string    // human-readable explanation
}

// Check looks for activity between receivedAt and receivedAt+Window, as far
// as now allows. Xurrent failures are returned as an error and must never
// be read as "none".
func (c *Checker) Check(ctx context.Context, receivedAt, now time.Time) (Verdict, error) {
	to := receivedAt.Add(c.cfg.Window)
	if now.Before(to) {
		to = now
	}
	acts, err := c.Activity(ctx, receivedAt, to)
	if err != nil {
		return Verdict{}, err
	}
	return Evaluate(acts, receivedAt, now, c.cfg.Window), nil
}

// Evaluate decides one voicemail from activity fetched for a range that
// covers [receivedAt, min(now, receivedAt+window)). Before the window has
// closed the result is activity or pending; after it, activity or none.
func Evaluate(acts []Activity, receivedAt, now time.Time, window time.Duration) Verdict {
	end := receivedAt.Add(window)
	for _, a := range acts {
		if !a.At.Before(receivedAt) && a.At.Before(end) && a.At.Before(now) {
			return Verdict{Status: StatusActivity, Activity: &a, Detail: a.Describe()}
		}
	}
	if now.Before(end) {
		return Verdict{Status: StatusPending}
	}
	return Verdict{Status: StatusNone, Detail: fmt.Sprintf("geen activiteit binnen %s", FormatDuration(window))}
}

// Earlier is a previous voicemail, used for the repeat-caller rule.
type Earlier struct {
	ID         int64
	ReceivedAt time.Time
	Caller     string
	Status     string
}

// RepeatOf returns the most recent earlier voicemail from the same caller,
// within the repeat window, that was followed by activity. A customer who
// calls again about the same problem is usually added to the existing
// request without a new note; without this rule the second voicemail would
// be reported as missed.
func RepeatOf(caller string, receivedAt time.Time, repeatWindow time.Duration, earlier []Earlier) *Earlier {
	if caller == "" || repeatWindow <= 0 {
		return nil
	}
	var best *Earlier
	for i := range earlier {
		e := &earlier[i]
		if e.Caller != caller || e.Status != StatusActivity {
			continue
		}
		if !e.ReceivedAt.Before(receivedAt) || receivedAt.Sub(e.ReceivedAt) > repeatWindow {
			continue
		}
		if best == nil || e.ReceivedAt.After(best.ReceivedAt) {
			best = e
		}
	}
	return best
}

var (
	quotedCaller = regexp.MustCompile(`(?im)^\s*from\s*:\s*"([^"\n]+)"`)
	angledCaller = regexp.MustCompile(`(?im)^\s*from\s*:.*<([^>\n]+)>`)
)

// CallerNumber extracts the caller's phone number from the PBX mail body,
// which has a line like `From:    "0657553610" <0657553610>`. It returns the
// digits in national form, or "" for an anonymous or unparsable caller.
func CallerNumber(emailText string) string {
	// The <...> part is the number; the quoted part is a display name that
	// is usually the number too, but may be a name.
	for _, re := range []*regexp.Regexp{angledCaller, quotedCaller} {
		if m := re.FindStringSubmatch(emailText); m != nil {
			if n := normalizeNumber(m[1]); n != "" {
				return n
			}
		}
	}
	return ""
}

func normalizeNumber(raw string) string {
	var digits strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	n := digits.String()
	switch {
	case strings.HasPrefix(n, "0031"):
		n = "0" + n[4:]
	case strings.HasPrefix(n, "31") && len(n) == 11:
		n = "0" + n[2:]
	}
	// Anything this short is an extension or a placeholder, not a caller.
	if len(n) < 6 {
		return ""
	}
	return n
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func snippet(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// FormatDuration renders whole hours as "1 uur" for Dutch messages.
func FormatDuration(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		return fmt.Sprintf("%d uur", int(d/time.Hour))
	}
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%d minuten", int(d/time.Minute))
	}
	return d.String()
}
