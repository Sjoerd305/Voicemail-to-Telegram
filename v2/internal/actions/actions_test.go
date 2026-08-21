package actions

import (
	"testing"

	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/config"
)

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Sjoerd van Dijk":        "sjoerd_van_dijk",
		"Remco Hoekstra (prive)": "remco_hoekstra_prive",
		"Servicedesk":            "servicedesk",
		"  Rare--Naam!! 2 ":      "rare_naam_2",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolvePhone(t *testing.T) {
	r := New(&config.Config{PhoneNumbers: map[string]string{
		"Sjoerd van Dijk":         "0611111111",
		"Sjoerd van Dijk (prive)": "0622222222",
		"Roel Lambers":            "0633333333",
		"Servicedesk":             "0644444444",
	}})

	cases := []struct {
		in, wantKey string
		wantFound   bool
	}{
		{"Sjoerd van Dijk", "Sjoerd van Dijk", true}, // exact key (web UI)
		{"sjoerd_van_dijk", "Sjoerd van Dijk", true}, // telegram slug
		{"sjoerd_van_dijk_prive", "Sjoerd van Dijk (prive)", true},
		{"sjoerd", "Sjoerd van Dijk", true}, // first-name shortcut skips (prive)
		{"roel", "Roel Lambers", true},
		{"servicedesk", "Servicedesk", true},
		{"onbekend", "", false},
		{"s", "", false}, // prefix of multiple plain names -> ambiguous
	}
	for _, c := range cases {
		key, num, found := r.resolvePhone(c.in)
		if found != c.wantFound || key != c.wantKey {
			t.Errorf("resolvePhone(%q) = (%q, %q, %v), want key %q found %v",
				c.in, key, num, found, c.wantKey, c.wantFound)
		}
	}
}
