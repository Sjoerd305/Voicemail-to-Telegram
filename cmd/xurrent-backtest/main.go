// Command xurrent-backtest runs the Xurrent activity check against the
// voicemails already in the database, to see how often a voicemail would
// have been reported as missed before turning the check on. It only reads:
// the database is opened read-only, nothing is sent to Telegram and nothing
// is written to Xurrent.
//
//	xurrent-backtest -config /config/config.yaml -since 2026-09-01 -weekdays -hours 08:00-17:00
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Sjoerd305/Voicemail-to-Telegram/internal/config"
	"github.com/Sjoerd305/Voicemail-to-Telegram/internal/store"
	"github.com/Sjoerd305/Voicemail-to-Telegram/internal/xurrent"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type result struct {
	vm      store.Voicemail
	caller  string
	verdict xurrent.Verdict
	delay   time.Duration // receipt to first activity
}

func run() error {
	configPath := flag.String("config", "config/config.yaml", "path to config file")
	dbPath := flag.String("db", "", "database file (default: storage.database from the config)")
	sinceStr := flag.String("since", time.Now().AddDate(0, 0, -30).Format("2006-01-02"), "first day to check (YYYY-MM-DD)")
	untilStr := flag.String("until", "", "day after the last day to check (YYYY-MM-DD, default: now)")
	window := flag.Duration("window", 0, "activity window (default: xurrent.window from the config)")
	weekdays := flag.Bool("weekdays", false, "only voicemails received Monday to Friday")
	hours := flag.String("hours", "", "only voicemails received in this local time range, e.g. 08:00-17:00")
	control := flag.Int("control", 500, "random windows without a voicemail, to measure activity that happens anyway (0 = off)")
	csvPath := flag.String("csv", "", "also write every result to this CSV file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	xc := cfg.Xurrent
	if xc.Token == "" || xc.Account == "" || !xc.HasPeople() {
		return fmt.Errorf("set xurrent.token, xurrent.account and xurrent.people in the config (enabled may stay false)")
	}
	if *window > 0 {
		xc.Window = *window
	}
	since, err := time.ParseInLocation("2006-01-02", *sinceStr, time.Local)
	if err != nil {
		return fmt.Errorf("-since: %w", err)
	}
	until := time.Now()
	if *untilStr != "" {
		if until, err = time.ParseInLocation("2006-01-02", *untilStr, time.Local); err != nil {
			return fmt.Errorf("-until: %w", err)
		}
	}
	inHours, err := parseHours(*hours)
	if err != nil {
		return err
	}
	include := func(t time.Time) bool {
		t = t.In(time.Local)
		if *weekdays && (t.Weekday() == time.Saturday || t.Weekday() == time.Sunday) {
			return false
		}
		return inHours(t)
	}

	path := *dbPath
	if path == "" {
		path = cfg.Storage.Database
	}
	st, err := store.OpenReadOnly(path)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer st.Close()
	all, err := st.VoicemailsBetween(since, until)
	if err != nil {
		return err
	}

	ctx := context.Background()
	checker := xurrent.NewChecker(xc)
	people, err := checker.People(ctx)
	if err != nil {
		return fmt.Errorf("resolve people: %w", err)
	}
	fmt.Printf("Gevolgde personen (%d):\n", len(people))
	for _, label := range sortedValues(people) {
		fmt.Println("  " + label)
	}
	fmt.Printf("Venster %s, herhaalde oproep %s, periode %s t/m %s\n\n",
		xurrent.FormatDuration(xc.Window), xurrent.FormatDuration(xc.RepeatWindow),
		since.Format("2006-01-02"), until.Add(-time.Second).Format("2006-01-02"))

	// Fetch all activity of the period once (one call per request updated
	// since the start) and decide every voicemail from it locally.
	now := time.Now()
	to := until.Add(xc.Window)
	if now.Before(to) {
		to = now
	}
	checker.Progress = func(done, total int) {
		if done%50 == 0 || done == total {
			fmt.Fprintf(os.Stderr, "\rNotities ophalen: %d/%d meldingen", done, total)
			if done == total {
				fmt.Fprintln(os.Stderr)
			}
		}
	}
	acts, err := checker.Activity(ctx, since, to)
	if err != nil {
		return fmt.Errorf("fetch activity: %w", err)
	}
	fmt.Printf("%d keer activiteit gevonden in de periode\n\n", len(acts))

	var results []result
	var earlier []xurrent.Earlier
	for _, vm := range all {
		r := result{vm: vm, caller: xurrent.CallerNumber(vm.EmailText)}
		r.verdict = xurrent.Evaluate(acts, vm.ReceivedAt, now, xc.Window)
		if r.verdict.Activity != nil {
			r.delay = r.verdict.Activity.At.Sub(vm.ReceivedAt)
		}
		if r.verdict.Status == xurrent.StatusNone {
			if e := xurrent.RepeatOf(r.caller, vm.ReceivedAt, xc.RepeatWindow, earlier); e != nil {
				r.verdict = xurrent.Verdict{Status: xurrent.StatusActivity,
					Detail: fmt.Sprintf("herhaalde oproep; eerdere voicemail #%d van dit nummer is al opgepakt", e.ID)}
			}
		}
		// The repeat rule looks at all earlier voicemails, also those the
		// filters leave out of the report.
		earlier = append(earlier, xurrent.Earlier{ID: vm.ID, ReceivedAt: vm.ReceivedAt, Caller: r.caller, Status: r.verdict.Status})
		if include(vm.ReceivedAt) {
			results = append(results, r)
		}
	}

	printResults(results)
	printSummary(results, xc.Window)

	if *control > 0 {
		runControl(acts, all, since, to, xc.Window, *control, include)
	}
	if *csvPath != "" {
		if err := writeCSV(*csvPath, results); err != nil {
			return err
		}
		fmt.Printf("\nCSV geschreven naar %s\n", *csvPath)
	}
	return nil
}

func printResults(results []result) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tONTVANGEN\tBELLER\tSTATUS\tNA\tDETAIL")
	for _, r := range results {
		after := ""
		if r.verdict.Activity != nil {
			after = fmt.Sprintf("%dm", int(r.delay.Minutes()))
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", r.vm.ID,
			r.vm.ReceivedAt.In(time.Local).Format("Mon 02-01 15:04"),
			r.caller, r.verdict.Status, after, oneLine(r.verdict.Detail, 100))
	}
	_ = tw.Flush()
}

func printSummary(results []result, window time.Duration) {
	counts := map[string]int{}
	var delays []time.Duration
	for _, r := range results {
		counts[r.verdict.Status]++
		if r.verdict.Activity != nil {
			delays = append(delays, r.delay)
		}
	}
	fmt.Printf("\nTotaal %d voicemails: %d activiteit, %d geen activiteit, %d nog binnen venster, %d fout\n",
		len(results), counts[xurrent.StatusActivity], counts[xurrent.StatusNone],
		counts[xurrent.StatusPending], counts[xurrent.StatusError])
	if len(delays) > 0 {
		sort.Slice(delays, func(i, j int) bool { return delays[i] < delays[j] })
		pct := func(p float64) time.Duration { return delays[int(p*float64(len(delays)-1))] }
		fmt.Printf("Tijd tot eerste activiteit: mediaan %dm, 90%% binnen %dm, langste %dm (venster %s)\n",
			int(pct(.5).Minutes()), int(pct(.9).Minutes()), int(delays[len(delays)-1].Minutes()),
			xurrent.FormatDuration(window))
	}
}

// runControl samples random windows in which no voicemail came in. Activity
// there is unrelated to a voicemail; the share of such windows is the chance
// that a missed voicemail is masked by activity that happened anyway.
func runControl(acts []xurrent.Activity, vms []store.Voicemail, since, until time.Time, window time.Duration, n int, include func(time.Time) bool) {
	span := until.Sub(since) - window
	if span <= 0 {
		return
	}
	rng := rand.New(rand.NewSource(1)) // #nosec G404 -- sampling, not security
	hits, tried := 0, 0
	for attempts := 0; tried < n && attempts < n*50; attempts++ {
		t := since.Add(time.Duration(rng.Int63n(int64(span)))).Truncate(time.Minute)
		if !include(t) || nearVoicemail(t, window, vms) {
			continue
		}
		tried++
		if xurrent.Evaluate(acts, t, t.Add(window), window).Status == xurrent.StatusActivity {
			hits++
		}
	}
	if tried == 0 {
		fmt.Println("\nControle: geen vensters zonder voicemail gevonden.")
		return
	}
	fmt.Printf("\nControle: in %d van %d willekeurige vensters zonder voicemail (%.0f%%) was er toch activiteit.\n",
		hits, tried, 100*float64(hits)/float64(tried))
	fmt.Println("Dat is ongeveer de kans dat een gemiste voicemail onopgemerkt blijft doordat er toevallig iets anders gebeurde.")
}

// nearVoicemail reports whether a voicemail came in within one window
// before or during [t, t+window), i.e. whether activity there could be a
// response to one.
func nearVoicemail(t time.Time, window time.Duration, vms []store.Voicemail) bool {
	for _, vm := range vms {
		if vm.ReceivedAt.After(t.Add(-window)) && vm.ReceivedAt.Before(t.Add(window)) {
			return true
		}
	}
	return false
}

func writeCSV(path string, results []result) error {
	f, err := os.Create(path) // #nosec G304 -- path is the operator's -csv flag
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"id", "received_at", "caller", "subject", "status", "minutes_to_activity", "request_id", "detail"})
	for _, r := range results {
		mins, reqID := "", ""
		if a := r.verdict.Activity; a != nil {
			mins = strconv.Itoa(int(r.delay.Minutes()))
			if a.RequestID != 0 {
				reqID = strconv.Itoa(a.RequestID)
			}
		}
		_ = w.Write([]string{strconv.FormatInt(r.vm.ID, 10), r.vm.ReceivedAt.In(time.Local).Format(time.RFC3339),
			r.caller, r.vm.Subject, r.verdict.Status, mins, reqID, r.verdict.Detail})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// parseHours turns "08:00-17:00" into a filter on local time of day.
func parseHours(s string) (func(time.Time) bool, error) {
	if s == "" {
		return func(time.Time) bool { return true }, nil
	}
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return nil, fmt.Errorf("-hours: want HH:MM-HH:MM, got %q", s)
	}
	from, err1 := time.Parse("15:04", strings.TrimSpace(a))
	to, err2 := time.Parse("15:04", strings.TrimSpace(b))
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("-hours: want HH:MM-HH:MM, got %q", s)
	}
	fromMin, toMin := from.Hour()*60+from.Minute(), to.Hour()*60+to.Minute()
	return func(t time.Time) bool {
		m := t.Hour()*60 + t.Minute()
		return m >= fromMin && m < toMin
	}, nil
}

func sortedValues(m map[int]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
