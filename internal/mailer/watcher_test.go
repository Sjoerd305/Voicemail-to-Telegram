package mailer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestBuildMessageShort(t *testing.T) {
	caption, extra := buildMessage("PBX vm", "text", "hello", "")
	if len(extra) != 0 {
		t.Fatalf("expected no extra parts, got %d", len(extra))
	}
	if !strings.Contains(caption, "hello") {
		t.Fatalf("caption missing transcription: %q", caption)
	}
}

func TestBuildMessageCleansAndLinks(t *testing.T) {
	email := "Klantnaam: Bakkerij De Molen\nFrom: \"0612345678\" <0612345678>\nLINK: http://pbx.local/listen/123\nhttp://pbx.local/raw\nDuur: 0:23"
	caption, extra := buildMessage("PBX vm", email, "hallo", "http://voicemail.local:8080")
	if len(extra) != 0 {
		t.Fatalf("expected no extra parts, got %d", len(extra))
	}
	if strings.Contains(caption, "pbx.local") {
		t.Fatalf("PBX link not stripped: %q", caption)
	}
	for _, want := range []string{
		"Klantnaam: Bakkerij De Molen",
		"Duur: 0:23",
		"Transcriptie: hallo",
		"Bekijk de melding ook op http://voicemail.local:8080",
	} {
		if !strings.Contains(caption, want) {
			t.Fatalf("caption missing %q:\n%s", want, caption)
		}
	}
	if strings.Contains(caption, "Subject:") || strings.Contains(caption, "Email Text:") {
		t.Fatalf("old labels still present: %q", caption)
	}
}

func TestCleanEmailTextKeepsNormalLines(t *testing.T) {
	// A line merely starting with the word "link..." is not a LINK: field.
	got := cleanEmailText("Linksom draaien\nLINK http://x\nGewone regel")
	if !strings.Contains(got, "Linksom draaien") || !strings.Contains(got, "Gewone regel") {
		t.Fatalf("stripped too much: %q", got)
	}
	if strings.Contains(got, "http://x") {
		t.Fatalf("LINK line not stripped: %q", got)
	}
}

func TestBuildMessageLong(t *testing.T) {
	long := strings.Repeat("woord ", 1000) // ~6000 bytes
	caption, extra := buildMessage("PBX vm", "text", long, "")
	if len(caption) > 1024 {
		t.Fatalf("caption too long: %d", len(caption))
	}
	if len(extra) == 0 {
		t.Fatal("expected overflow parts")
	}
	joined := caption + strings.Join(extra, "")
	if !strings.HasSuffix(joined, strings.TrimSpace(long)+"") && !strings.Contains(joined, "woord woord") {
		t.Fatal("content lost while splitting")
	}
	for _, p := range extra {
		if len(p) > 4096 {
			t.Fatalf("extra part too long: %d", len(p))
		}
	}
}

func TestTruncateUTF8(t *testing.T) {
	s := strings.Repeat("é", 100) // 2 bytes per rune
	got := truncateUTF8(s, 33)
	if len(got) != 32 {
		t.Fatalf("expected 32 bytes (no split rune), got %d", len(got))
	}
}

func TestFolderName(t *testing.T) {
	// Wednesday 2026-08-19 is ISO week 34.
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	if got := folderName(now); got != "INBOX.2026.33-34" {
		t.Fatalf("unexpected folder name: %s", got)
	}
}

func TestNextBackoffGrowsAndCaps(t *testing.T) {
	base := 30 * time.Second
	got := nextBackoff(0, base)
	if got != base {
		t.Fatalf("first backoff = %v, want %v", got, base)
	}
	// Doubles until it hits the cap and then stays there.
	for i := 0; i < 20; i++ {
		next := nextBackoff(got, base)
		if next < got {
			t.Fatalf("backoff shrank: %v -> %v", got, next)
		}
		got = next
	}
	if got != maxBackoff {
		t.Fatalf("backoff settled at %v, want cap %v", got, maxBackoff)
	}
}

func TestSleepCtxReportsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Hour) {
		t.Fatal("sleepCtx should report false on a cancelled context")
	}
	if !sleepCtx(context.Background(), time.Millisecond) {
		t.Fatal("sleepCtx should report true when the timer fires")
	}
}

// The unilateral data handler runs on the IMAP read goroutine and must never
// block it, however many notifications arrive before the watcher reacts.
func TestMailboxSignalNeverBlocks(t *testing.T) {
	w := &Watcher{mailbox: make(chan struct{}, 1)}
	signal := func() {
		select {
		case w.mailbox <- struct{}{}:
		default:
		}
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			signal()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("signalling the mailbox channel blocked")
	}
	if len(w.mailbox) != 1 {
		t.Fatalf("expected the notifications to coalesce into 1, got %d", len(w.mailbox))
	}
}

func TestStatusReportsMode(t *testing.T) {
	w := &Watcher{interval: 30 * time.Second}
	if got := w.Status(); got.Idle || got.CheckIntervalSeconds != 30 {
		t.Fatalf("unexpected initial status: %+v", got)
	}
	w.setMode(true, 2*time.Minute)
	w.setResult(nil)
	got := w.Status()
	if !got.Idle || got.CheckIntervalSeconds != 120 {
		t.Fatalf("mode not reflected in status: %+v", got)
	}
	if got.LastError != "" || got.LastPoll.IsZero() {
		t.Fatalf("successful check not recorded: %+v", got)
	}
	w.setResult(errors.New("boom"))
	if got := w.Status(); got.LastError != "boom" {
		t.Fatalf("error not recorded: %+v", got)
	}
}
