package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadExampleWithEnvExpansion(t *testing.T) {
	t.Setenv("IMAP_PASSWORD", "secret-from-env")
	t.Setenv("TELEGRAM_TOKEN", "token-from-env")
	// Starts with a YAML-reserved character; must not break parsing.
	t.Setenv("SSH_PASSWORD", "@ssh: #from *env")

	raw, err := os.ReadFile("../../config/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IMAP.Password != "secret-from-env" {
		t.Fatalf("env expansion failed: %q", cfg.IMAP.Password)
	}
	if cfg.SSH.Password != "@ssh: #from *env" {
		t.Fatalf("special-char env expansion failed: %q", cfg.SSH.Password)
	}
	if cfg.Telegram.ChatID != -123456789 {
		t.Fatalf("chat id: %d", cfg.Telegram.ChatID)
	}
	if len(cfg.Commands) != 3 || cfg.Commands["deletevm"].Type != "ssh" {
		t.Fatalf("commands not parsed: %+v", cfg.Commands)
	}
	if cfg.PhoneNumbers["name1"] != "0612345678" {
		t.Fatalf("phone numbers not parsed: %+v", cfg.PhoneNumbers)
	}
	if cfg.Cleanup.Schedule != "0 9 * * 5" {
		t.Fatalf("cleanup schedule: %q", cfg.Cleanup.Schedule)
	}
}
