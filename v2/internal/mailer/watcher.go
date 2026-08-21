// Package mailer polls the IMAP inbox for voicemail mails from the PBX,
// transcribes the attached audio and forwards everything to Telegram.
package mailer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/mail"

	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/config"
	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/store"
	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/transcribe"
)

// Notifier is the part of the Telegram bot the watcher needs.
type Notifier interface {
	SendVoicemail(caption string, extraParts []string, audio []byte) error
}

const (
	// dialTimeout bounds the TCP+TLS handshake. The library default is 30s,
	// long enough that a wedged connect visibly delays shutdown.
	dialTimeout = 15 * time.Second
	// maxBackoff caps the retry delay after connection failures. Retrying at
	// full speed against a server that is rate-limiting us only extends the
	// block, so consecutive failures back off.
	maxBackoff = 5 * time.Minute
	// minSessionUptime is how long a session must have lasted before it
	// counts as healthy and the backoff resets.
	minSessionUptime = time.Minute
)

type Watcher struct {
	cfg         *config.Config
	store       *store.Store
	transcriber *transcribe.Transcriber
	notifier    Notifier

	// mailbox carries server-pushed "mailbox changed" notifications. It is
	// buffered so the IMAP read goroutine never blocks signalling it, and a
	// depth of one is enough: the reaction is always the same full search.
	mailbox chan struct{}

	mu        sync.Mutex
	lastPoll  time.Time
	lastError string
	idle      bool
	interval  time.Duration
}

func NewWatcher(cfg *config.Config, st *store.Store, tr *transcribe.Transcriber, n Notifier) *Watcher {
	return &Watcher{
		cfg:         cfg,
		store:       st,
		transcriber: tr,
		notifier:    n,
		mailbox:     make(chan struct{}, 1),
		interval:    cfg.IMAP.PollInterval,
	}
}

type Status struct {
	LastPoll  time.Time `json:"last_poll"`
	LastError string    `json:"last_error"`
	// Idle reports whether the server pushes new-mail notifications.
	Idle bool `json:"idle"`
	// CheckIntervalSeconds is how long the dashboard should wait before
	// treating a missing check as "behind"; it differs between idle and
	// polling mode.
	CheckIntervalSeconds int `json:"check_interval_seconds"`
}

func (w *Watcher) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	return Status{
		LastPoll:             w.lastPoll,
		LastError:            w.lastError,
		Idle:                 w.idle,
		CheckIntervalSeconds: int(w.interval.Seconds()),
	}
}

func (w *Watcher) setResult(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastPoll = time.Now()
	if err != nil {
		w.lastError = err.Error()
	} else {
		w.lastError = ""
	}
}

func (w *Watcher) setMode(idle bool, interval time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.idle = idle
	w.interval = interval
}

// Run keeps a mail session alive until the context is cancelled, reconnecting
// with backoff whenever one dies.
func (w *Watcher) Run(ctx context.Context) {
	slog.Info("mail watcher started",
		"server", w.cfg.IMAP.Server,
		"use_idle", w.cfg.IMAP.UseIDLE,
		"poll_interval", w.cfg.IMAP.PollInterval,
		"idle_fallback_interval", w.cfg.IMAP.IdleFallbackInterval)

	var backoff time.Duration
	for {
		if ctx.Err() != nil {
			return
		}
		if backoff > 0 {
			slog.Warn("reconnecting to imap after failure", "in", backoff)
			if !sleepCtx(ctx, backoff) {
				return
			}
		}

		started := time.Now()
		err := w.session(ctx)
		if ctx.Err() != nil {
			return
		}
		w.setMode(false, w.cfg.IMAP.PollInterval)
		w.setResult(err)
		slog.Error("imap session ended", "err", err, "uptime", time.Since(started).Round(time.Second))

		// A session that ran for a while was healthy; only rapid-fire
		// failures should escalate the delay.
		if time.Since(started) > minSessionUptime {
			backoff = 0
		}
		backoff = nextBackoff(backoff, w.cfg.IMAP.PollInterval)
	}
}

// session logs in once and then holds that connection for as long as the
// server allows. Dialing and authenticating per check would mean roughly 2900
// logins a day at a 30s interval, which hosted providers throttle as a
// brute-force defence — the login, not the inbox check, is the expensive part.
func (w *Watcher) session(ctx context.Context) error {
	client, err := w.connect()
	if err != nil {
		return err
	}
	defer func() {
		// Best effort: on a broken connection both of these fail, and the
		// caller is already reconnecting.
		_ = client.Logout().Wait()
		_ = client.Close()
	}()

	useIdle := w.cfg.IMAP.UseIDLE && client.Caps().Has(imap.CapIdle)
	if w.cfg.IMAP.UseIDLE && !useIdle {
		slog.Warn("imap server does not advertise IDLE, falling back to polling",
			"server", w.cfg.IMAP.Server)
	}
	interval := w.cfg.IMAP.PollInterval
	if useIdle {
		interval = w.cfg.IMAP.IdleFallbackInterval
	}
	w.setMode(useIdle, interval)
	slog.Info("imap connected", "idle", useIdle, "check_interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		// Drain any pending notification *before* searching, not after: mail
		// that arrives while we are transcribing must leave the flag set so
		// the next wait returns immediately instead of swallowing it.
		select {
		case <-w.mailbox:
		default:
		}

		err := w.check(ctx, client)
		w.setResult(err)
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if useIdle {
			// While IDLE runs no other command may be sent, so the loop waits
			// here until the server pushes something, the safety-net ticker
			// fires, or we are shutting down.
			idleCmd, err := client.Idle()
			if err != nil {
				return fmt.Errorf("imap idle: %w", err)
			}
			select {
			case <-ctx.Done():
			case <-w.mailbox:
			case <-ticker.C:
			}
			if err := idleCmd.Close(); err != nil {
				return fmt.Errorf("stop imap idle: %w", err)
			}
		} else {
			select {
			case <-ctx.Done():
			case <-ticker.C:
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

func (w *Watcher) connect() (*imapclient.Client, error) {
	addr := net.JoinHostPort(w.cfg.IMAP.Server, strconv.Itoa(w.cfg.IMAP.Port))
	client, err := imapclient.DialTLS(addr, &imapclient.Options{
		Dialer: &net.Dialer{Timeout: dialTimeout},
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			// This runs on the client's read goroutine and blocks it, so it
			// may only signal — never do the actual work here.
			Mailbox: func(*imapclient.UnilateralDataMailbox) {
				select {
				case w.mailbox <- struct{}{}:
				default:
				}
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("dial imap: %w", err)
	}
	if err := client.Login(w.cfg.IMAP.Email, w.cfg.IMAP.Password).Wait(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("imap login: %w", err)
	}
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("select inbox: %w", err)
	}
	return client, nil
}

// check searches the already-selected INBOX and processes what it finds.
// Returning an error tears the session down, so the caller reconnects.
func (w *Watcher) check(ctx context.Context, client *imapclient.Client) error {
	criteria := &imap.SearchCriteria{
		NotFlag: []imap.Flag{imap.FlagSeen},
		Header: []imap.SearchCriteriaHeaderField{
			{Key: "Subject", Value: w.cfg.IMAP.SubjectFilter},
		},
	}
	searchData, err := client.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return fmt.Errorf("imap search: %w", err)
	}
	uids := searchData.AllUIDs()
	if len(uids) == 0 {
		return nil
	}
	slog.Info("found unseen voicemail mails", "count", len(uids))

	for _, uid := range uids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := w.processMessage(ctx, client, uid); err != nil {
			slog.Error("failed to process mail", "uid", uid, "err", err)
			w.store.LogEvent("error", fmt.Sprintf("mail uid %d: %v", uid, err))
			continue
		}
		// Only mark seen after successful processing, so failures are
		// retried on the next check.
		uidSet := imap.UIDSetNum(uid)
		if err := client.Store(uidSet, &imap.StoreFlags{
			Op:     imap.StoreFlagsAdd,
			Flags:  []imap.Flag{imap.FlagSeen},
			Silent: true,
		}, nil).Close(); err != nil {
			slog.Error("failed to mark mail seen", "uid", uid, "err", err)
		}
	}
	return nil
}

func nextBackoff(current, base time.Duration) time.Duration {
	if current <= 0 {
		return base
	}
	if next := current * 2; next < maxBackoff {
		return next
	}
	return maxBackoff
}

// sleepCtx waits for d, reporting false if the context was cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (w *Watcher) processMessage(ctx context.Context, client *imapclient.Client, uid imap.UID) error {
	uidSet := imap.UIDSetNum(uid)
	bodySection := &imap.FetchItemBodySection{Peek: true}
	msgs, err := client.Fetch(uidSet, &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{bodySection},
	}).Collect()
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	if len(msgs) == 0 {
		return fmt.Errorf("fetch returned no message")
	}
	raw := msgs[0].FindBodySection(bodySection)
	if raw == nil {
		return fmt.Errorf("fetch returned no body")
	}

	parsed, err := parseMessage(raw)
	if err != nil {
		return fmt.Errorf("parse mail: %w", err)
	}

	// Duplicate guard: survives restarts and unseen-flag races.
	if done, err := w.store.IsProcessed(parsed.MessageID); err != nil {
		return err
	} else if done {
		slog.Info("skipping already processed mail", "message_id", parsed.MessageID)
		return nil
	}

	transcription := ""
	if len(parsed.Audio) > 0 {
		transcription, err = w.transcriber.Transcribe(ctx, parsed.Audio)
		if err != nil {
			slog.Error("transcription failed", "err", err)
			transcription = "(transcriptie mislukt)"
		}
	}

	caption, extra := buildMessage(parsed.Subject, parsed.Text, transcription, w.cfg.Web.PublicURL)
	if err := w.notifier.SendVoicemail(caption, extra, parsed.Audio); err != nil {
		return fmt.Errorf("send telegram: %w", err)
	}

	audioPath := ""
	if len(parsed.Audio) > 0 {
		audioPath = filepath.Join(w.cfg.Storage.AudioDir,
			fmt.Sprintf("%d.wav", time.Now().UnixNano()))
		if err := os.WriteFile(audioPath, parsed.Audio, 0o600); err != nil {
			slog.Error("failed to store audio file", "err", err)
			audioPath = ""
		}
	}

	vm := &store.Voicemail{
		ReceivedAt:    parsed.Date,
		Subject:       parsed.Subject,
		EmailText:     parsed.Text,
		Transcription: transcription,
		AudioPath:     audioPath,
		MessageID:     parsed.MessageID,
	}
	if err := w.store.SaveVoicemail(vm); err != nil {
		return fmt.Errorf("save voicemail: %w", err)
	}
	w.store.LogEvent("voicemail", parsed.Subject)
	slog.Info("voicemail processed", "subject", parsed.Subject)
	return nil
}

type parsedMail struct {
	Subject   string
	MessageID string
	Date      time.Time
	Text      string
	Audio     []byte
}

func parseMessage(raw []byte) (*parsedMail, error) {
	mr, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	out := &parsedMail{Date: time.Now()}
	out.Subject, _ = mr.Header.Subject()
	out.MessageID, _ = mr.Header.MessageID()
	if d, err := mr.Header.Date(); err == nil && !d.IsZero() {
		out.Date = d
	}

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Tolerate malformed sub-parts; keep what we already have.
			slog.Warn("skipping malformed mime part", "err", err)
			break
		}
		switch h := part.Header.(type) {
		case *mail.InlineHeader:
			ct, _, _ := h.ContentType()
			switch {
			case strings.HasPrefix(ct, "text/plain") && out.Text == "":
				body, _ := io.ReadAll(part.Body)
				out.Text = strings.TrimSpace(string(body))
			case strings.HasPrefix(ct, "audio/") && out.Audio == nil:
				out.Audio, _ = io.ReadAll(part.Body)
			}
		case *mail.AttachmentHeader:
			ct, _, _ := h.ContentType()
			filename, _ := h.Filename()
			if out.Audio == nil && (strings.HasPrefix(ct, "audio/") ||
				strings.HasSuffix(strings.ToLower(filename), ".wav")) {
				out.Audio, _ = io.ReadAll(part.Body)
			}
		}
	}
	return out, nil
}

// cleanEmailText strips noise from the PBX mail body before it goes to
// Telegram: "LINK: ..." lines and bare-URL lines (the PBX's own listen
// link, which nobody uses from the chat).
func cleanEmailText(text string) string {
	var keep []string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.ToLower(strings.TrimSpace(line))
		if strings.HasPrefix(trimmed, "link") &&
			(len(trimmed) == 4 || strings.ContainsAny(string(trimmed[4]), ": =")) {
			continue
		}
		if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}

// buildMessage produces the Telegram caption (max 1024 chars) plus any
// overflow as separate text messages. This replaces the old behaviour of
// re-sending the audio file once per text part.
func buildMessage(subject, emailText, transcription, publicURL string) (string, []string) {
	var b strings.Builder
	if subject != "" {
		b.WriteString(subject)
	}
	if body := cleanEmailText(emailText); body != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(body)
	}
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	b.WriteString("Transcriptie: ")
	if transcription != "" {
		b.WriteString(transcription)
	} else {
		b.WriteString("(geen)")
	}
	if publicURL != "" {
		b.WriteString("\n\nBekijk de melding ook op ")
		b.WriteString(publicURL)
	}
	full := b.String()
	const captionLimit = 1024
	if len(full) <= captionLimit {
		return full, nil
	}
	caption := truncateUTF8(full, captionLimit)
	rest := full[len(caption):]
	var extra []string
	const msgLimit = 4096
	for len(rest) > 0 {
		part := truncateUTF8(rest, msgLimit)
		extra = append(extra, part)
		rest = rest[len(part):]
	}
	return caption, extra
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}
