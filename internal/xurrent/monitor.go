package xurrent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Sjoerd305/Voicemail-to-Telegram/internal/config"
	"github.com/Sjoerd305/Voicemail-to-Telegram/internal/store"
)

// Notifier is the part of the Telegram bot the monitor needs.
type Notifier interface {
	SendMessage(text string) error
}

// Monitor checks pending voicemails until they are followed by activity or
// their window has closed. Only voicemails saved with status pending are
// looked at, so turning the feature on does not report on old ones.
type Monitor struct {
	checker  *Checker
	cfg      config.Xurrent
	store    *store.Store
	notifier Notifier
}

func NewMonitor(cfg config.Xurrent, st *store.Store, n Notifier) *Monitor {
	return &Monitor{checker: NewChecker(cfg), cfg: cfg, store: st, notifier: n}
}

const (
	sweepInterval = 5 * time.Minute
	// giveUpAfter: a voicemail whose window closed this long ago and still
	// cannot be checked is marked error rather than retried forever.
	giveUpAfter = 24 * time.Hour
)

func (m *Monitor) Run(ctx context.Context) {
	if _, err := m.checker.People(ctx); err != nil {
		// Not fatal: every sweep tries again, and nothing is reported as
		// missed while Xurrent cannot be reached.
		slog.Error("xurrent: resolving people failed", "err", err)
		m.store.LogEvent("error", fmt.Sprintf("Xurrent: personen/teams ophalen mislukt: %v", err))
	}
	m.sweep(ctx)
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.sweep(ctx)
		}
	}
}

func (m *Monitor) sweep(ctx context.Context) {
	m.sweepAt(ctx, time.Now())
}

// sweepAt fetches the activity since the oldest pending voicemail once and
// decides every pending voicemail from it.
func (m *Monitor) sweepAt(ctx context.Context, now time.Time) {
	pending, err := m.store.XurrentPending()
	if err != nil {
		slog.Error("xurrent: list pending", "err", err)
		return
	}
	if len(pending) == 0 {
		return
	}
	from := pending[0].ReceivedAt // oldest first
	to := now
	if last := pending[len(pending)-1].ReceivedAt.Add(m.cfg.Window); last.Before(to) {
		to = last
	}
	acts, err := m.checker.Activity(ctx, from, to)
	for _, vm := range pending {
		if ctx.Err() != nil {
			return
		}
		m.decide(vm, acts, err, now)
	}
}

func (m *Monitor) decide(vm store.Voicemail, acts []Activity, fetchErr error, now time.Time) {
	v := Evaluate(acts, vm.ReceivedAt, now, m.cfg.Window)
	if fetchErr != nil {
		slog.Warn("xurrent check failed", "voicemail", vm.ID, "err", fetchErr)
		if now.Sub(vm.ReceivedAt.Add(m.cfg.Window)) < giveUpAfter {
			return // retried on the next sweep
		}
		v = Verdict{Status: StatusError, Detail: "Xurrent kon niet worden gecontroleerd: " + fetchErr.Error()}
	}
	if v.Status == StatusPending {
		return
	}
	if v.Status == StatusNone {
		if e := m.repeatOf(vm); e != nil {
			v = Verdict{Status: StatusActivity, Detail: fmt.Sprintf("herhaalde oproep; eerdere voicemail #%d van dit nummer is al opgepakt", e.ID)}
		}
	}
	reqID := 0
	if v.Activity != nil {
		reqID = v.Activity.RequestID
	}
	if err := m.store.SetXurrent(vm.ID, v.Status, v.Detail, reqID); err != nil {
		slog.Error("xurrent: save result", "voicemail", vm.ID, "err", err)
		return
	}
	slog.Info("xurrent check", "voicemail", vm.ID, "status", v.Status, "detail", v.Detail)
	switch v.Status {
	case StatusActivity:
		m.store.LogEvent("xurrent", fmt.Sprintf("#%d: %s", vm.ID, v.Detail))
	case StatusNone:
		m.store.LogEvent("xurrent", fmt.Sprintf("#%d: geen Xurrent-activiteit binnen %s", vm.ID, FormatDuration(m.cfg.Window)))
		who := ""
		if l := m.checker.Label(); l != "" {
			who = " van " + l
		}
		m.notify(fmt.Sprintf("⚠️ Geen Xurrent-activiteit%s binnen %s na de voicemail van %s%s.",
			who, FormatDuration(m.cfg.Window), vm.ReceivedAt.In(time.Local).Format("15:04"), describeCaller(vm)))
	case StatusError:
		m.store.LogEvent("error", fmt.Sprintf("#%d: %s", vm.ID, v.Detail))
		m.notify(fmt.Sprintf("⚠️ Kon in Xurrent niet controleren of de voicemail van %s%s is opgepakt.",
			vm.ReceivedAt.In(time.Local).Format("02-01 15:04"), describeCaller(vm)))
	}
}

// repeatOf applies the repeat-caller rule against the voicemails before vm.
func (m *Monitor) repeatOf(vm store.Voicemail) *Earlier {
	caller := CallerNumber(vm.EmailText)
	if caller == "" || m.cfg.RepeatWindow <= 0 {
		return nil
	}
	page, err := m.store.ListVoicemails(store.ListOptions{
		Since: vm.ReceivedAt.Add(-m.cfg.RepeatWindow),
		Until: vm.ReceivedAt,
		Limit: store.MaxPageSize,
	})
	if err != nil {
		slog.Error("xurrent: list earlier voicemails", "err", err)
		return nil
	}
	earlier := make([]Earlier, 0, len(page.Items))
	for _, e := range page.Items {
		earlier = append(earlier, Earlier{ID: e.ID, ReceivedAt: e.ReceivedAt, Caller: CallerNumber(e.EmailText), Status: e.XurrentStatus})
	}
	return RepeatOf(caller, vm.ReceivedAt, m.cfg.RepeatWindow, earlier)
}

func (m *Monitor) notify(text string) {
	if err := m.notifier.SendMessage(text); err != nil {
		slog.Error("xurrent: telegram notify", "err", err)
	}
}

func describeCaller(vm store.Voicemail) string {
	var parts []string
	if c := customerName(vm.EmailText); c != "" {
		parts = append(parts, c)
	}
	if n := CallerNumber(vm.EmailText); n != "" {
		parts = append(parts, n)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// customerName reads the "Klantnaam: ..." line of the PBX mail.
func customerName(emailText string) string {
	for _, line := range strings.Split(emailText, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), "klantnaam") {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
