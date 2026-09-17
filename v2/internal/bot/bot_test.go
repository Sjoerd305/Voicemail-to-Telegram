package bot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/config"
)

func TestInfoMessage(t *testing.T) {
	dir := t.TempDir()
	customers := filepath.Join(dir, "customers.json")
	if err := os.WriteFile(customers, []byte(`{"bakkerij":"info","garage":"info"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	infoFile := filepath.Join(dir, "info.txt")
	if err := os.WriteFile(infoFile, []byte("Bij twijfel: bel de servicedesk.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	b := &Bot{cfg: &config.Config{
		Commands: map[string]config.Command{
			"deletevm": {Success: "Voicemail verwijderd.", Description: "Voicemail verwijderen van de centrale"},
			"vivia":    {Success: "Storingsdienst naar Vivia"},
		},
		PhoneNumbers: map[string]string{
			"Sjoerd van Dijk": "0611111111",
			"Roel Lambers":    "0622222222",
		},
		CustomersFile: customers,
		InfoFile:      infoFile,
	}}

	msg := b.infoMessage()
	for _, want := range []string{
		"/info – Toon dit bericht",
		"/deletevm – Voicemail verwijderen van de centrale", // description wins
		"/vivia – Storingsdienst naar Vivia",                // falls back to success
		"/sjoerd_van_dijk – Sjoerd van Dijk",
		"/roel_lambers – Roel Lambers",
		"Klantinfo: /bakkerij, /garage",
		"Bij twijfel: bel de servicedesk.", // info_file appended
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("infoMessage missing %q\n---\n%s", want, msg)
		}
	}
}

func TestInfoMessageMinimalConfig(t *testing.T) {
	b := &Bot{cfg: &config.Config{}}
	msg := b.infoMessage()
	if !strings.Contains(msg, "/info") || strings.Contains(msg, "Klantinfo") {
		t.Errorf("unexpected minimal info message:\n%s", msg)
	}
}

func TestSenderName(t *testing.T) {
	cases := []struct {
		user *tgbotapi.User
		want string
	}{
		{&tgbotapi.User{FirstName: "Sjoerd", LastName: "van Dijk", UserName: "sjoerd305"}, "Sjoerd van Dijk"},
		{&tgbotapi.User{FirstName: "Sjoerd"}, "Sjoerd"},
		// No name set at all: fall back to the @handle instead of "".
		{&tgbotapi.User{UserName: "sjoerd305"}, "@sjoerd305"},
		{&tgbotapi.User{}, "onbekend"},
		{nil, "onbekend"},
	}
	for _, c := range cases {
		if got := senderName(c.user); got != c.want {
			t.Errorf("senderName(%+v) = %q, want %q", c.user, got, c.want)
		}
	}
}

func TestMenuCommands(t *testing.T) {
	b := &Bot{cfg: &config.Config{
		Commands: map[string]config.Command{
			"deletevm": {Success: "Voicemail verwijderd.", Description: "Voicemail verwijderen van de centrale"},
			"vivia":    {Success: "Storingsdienst naar Vivia"},
			"Bad-Name": {Success: "Should be dropped"},
			"info":     {Success: "Duplicate of the fixed command"},
			"x":        {Success: "ok"},
		},
		PhoneNumbers: map[string]string{
			"Sjoerd van Dijk": "0611111111",
			"Roel Lambers":    "0622222222",
		},
	}}

	got := b.menuCommands()
	want := []tgbotapi.BotCommand{
		{Command: "info", Description: "Toon alle commando's"},
		{Command: "storingsdienst", Description: "Storingsdienst-commando's"},
		{Command: "deletevm", Description: "Voicemail verwijderen van de centrale"},
		{Command: "vivia", Description: "Storingsdienst naar Vivia"},
		{Command: "x", Description: "ok"},
		{Command: "roel_lambers", Description: "Storingsdienst naar Roel Lambers"},
		{Command: "sjoerd_van_dijk", Description: "Storingsdienst naar Sjoerd van Dijk"},
	}
	// "ok" is under Telegram's 3-character minimum and is padded.
	want[4].Description = "/x"
	if len(got) != len(want) {
		t.Fatalf("got %d commands, want %d:\n%+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("command %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestMenuCommandsCap(t *testing.T) {
	cfg := &config.Config{Commands: map[string]config.Command{}}
	for i := 0; i < 150; i++ {
		cfg.Commands[fmt.Sprintf("cmd%03d", i)] = config.Command{Success: "ok ok"}
	}
	if got := (&Bot{cfg: cfg}).menuCommands(); len(got) != maxMenuCommands {
		t.Errorf("got %d commands, want %d", len(got), maxMenuCommands)
	}
}
