package bot

import (
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
