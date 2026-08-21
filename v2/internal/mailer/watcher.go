// Package mailer polls the IMAP inbox for voicemail mails from the PBX,
// transcribes the attached audio and forwards everything to Telegram.
package mailer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

type Watcher struct {
	cfg         *config.Config
	store       *store.Store
	transcriber *transcribe.Transcriber
	notifier    Notifier

	lastPoll  time.Time
	lastError string
}

func NewWatcher(cfg *config.Config, st *store.Store, tr *transcribe.Transcriber, n Notifier) *Watcher {
	return &Watcher{cfg: cfg, store: st, transcriber: tr, notifier: n}
}

type Status struct {
	LastPoll  time.Time `json:"last_poll"`
	LastError string    `json:"last_error"`
}

func (w *Watcher) Status() Status {
	return Status{LastPoll: w.lastPoll, LastError: w.lastError}
}

// Run polls until the context is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	slog.Info("mail watcher started",
		"server", w.cfg.IMAP.Server, "interval", w.cfg.IMAP.PollInterval)
	ticker := time.NewTicker(w.cfg.IMAP.PollInterval)
	defer ticker.Stop()
	for {
		if err := w.poll(ctx); err != nil {
			w.lastError = err.Error()
			slog.Error("mail poll failed", "err", err)
		} else {
			w.lastError = ""
		}
		w.lastPoll = time.Now()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Watcher) poll(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", w.cfg.IMAP.Server, w.cfg.IMAP.Port)
	client, err := imapclient.DialTLS(addr, nil)
	if err != nil {
		return fmt.Errorf("dial imap: %w", err)
	}
	defer client.Close()

	if err := client.Login(w.cfg.IMAP.Email, w.cfg.IMAP.Password).Wait(); err != nil {
		return fmt.Errorf("imap login: %w", err)
	}
	defer client.Logout()

	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		return fmt.Errorf("select inbox: %w", err)
	}

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
		// retried on the next poll.
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
		if err := os.WriteFile(audioPath, parsed.Audio, 0o644); err != nil {
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
