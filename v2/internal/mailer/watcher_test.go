package mailer

import (
	"strings"
	"testing"
	"time"
)

func TestBuildMessageShort(t *testing.T) {
	caption, extra := buildMessage("PBX vm", "text", "hello")
	if len(extra) != 0 {
		t.Fatalf("expected no extra parts, got %d", len(extra))
	}
	if !strings.Contains(caption, "hello") {
		t.Fatalf("caption missing transcription: %q", caption)
	}
}

func TestBuildMessageLong(t *testing.T) {
	long := strings.Repeat("woord ", 1000) // ~6000 bytes
	caption, extra := buildMessage("PBX vm", "text", long)
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
