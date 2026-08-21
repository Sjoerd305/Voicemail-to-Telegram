package config

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type IMAP struct {
	Server   string `yaml:"server"`
	Port     int    `yaml:"port"`
	Email    string `yaml:"email"`
	Password string `yaml:"password"`
	// Subject substring that identifies voicemail mails, e.g. "PBX".
	SubjectFilter string        `yaml:"subject_filter"`
	PollInterval  time.Duration `yaml:"poll_interval"`
}

type Telegram struct {
	Token  string `yaml:"token"`
	ChatID int64  `yaml:"chat_id"`
}

type Transcription struct {
	Enabled         bool   `yaml:"enabled"`
	Language        string `yaml:"language"`
	CredentialsFile string `yaml:"google_credentials"`
	// Optional. Needed only for voicemails longer than ~60s; shorter audio is
	// transcribed inline without a bucket.
	GCSBucket string `yaml:"gcs_bucket"`
}

type SSH struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Command is a chat/web command that either runs a command over SSH on the PBX
// or calls an HTTP URL.
type Command struct {
	Type    string `yaml:"type"` // "ssh" or "http"
	Command string `yaml:"command,omitempty"`
	URL     string `yaml:"url,omitempty"`
	Success string `yaml:"success"`
	Error   string `yaml:"error"`
	// Primary marks a frequently used command; the web UI shows it as a
	// prominent button at the top of the actions panel.
	Primary bool `yaml:"primary,omitempty"`
	// Group places the command in a collapsible group in the web UI, e.g.
	// "storingsdienst" to join the phone-number switches.
	Group string `yaml:"group,omitempty"`
	// Description is shown in the dynamic /info command list; falls back to
	// the success message when empty.
	Description string `yaml:"description,omitempty"`
}

type Cleanup struct {
	Enabled  bool   `yaml:"enabled"`
	Schedule string `yaml:"schedule"` // cron expression
	Timezone string `yaml:"timezone"`
}

// GoogleAuth puts the whole dashboard behind a Google sign-in screen.
// Enabled when client_id is set; allowed_domains controls who may log in.
type GoogleAuth struct {
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	// AllowedDomains entries are email domains ("bedrijf.nl") or, when they
	// contain an @, individual addresses ("extern@gmail.com").
	AllowedDomains []string `yaml:"allowed_domains"`
}

type Web struct {
	Enabled  bool   `yaml:"enabled"`
	Listen   string `yaml:"listen"`
	Password string `yaml:"password"` // optional basic auth (user: admin); ignored when google_auth is enabled
	// PublicURL is the address where users reach the dashboard, e.g.
	// "http://192.168.1.5:8080". When set, voicemail messages in Telegram
	// end with a link to it. Required when google_auth is enabled (it forms
	// the OAuth redirect URL).
	PublicURL  string     `yaml:"public_url"`
	GoogleAuth GoogleAuth `yaml:"google_auth"`
	// AllowUnauthenticated must be set explicitly to run a publicly
	// reachable (https public_url) dashboard without any authentication.
	AllowUnauthenticated bool `yaml:"allow_unauthenticated"`
}

type Storage struct {
	Database string `yaml:"database"`
	AudioDir string `yaml:"audio_dir"`
}

type Config struct {
	IMAP          IMAP               `yaml:"imap"`
	Telegram      Telegram           `yaml:"telegram"`
	Transcription Transcription      `yaml:"transcription"`
	PBXIP         string             `yaml:"pbx_ip"`
	SSH           SSH                `yaml:"ssh"`
	Commands      map[string]Command `yaml:"commands"`
	PhoneNumbers  map[string]string  `yaml:"phone_numbers"`
	Cleanup       Cleanup            `yaml:"cleanup"`
	Web           Web                `yaml:"web"`
	Storage       Storage            `yaml:"storage"`
	InfoFile      string             `yaml:"info_file"`
	CustomersFile string             `yaml:"customers_file"`
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- path comes from the -config flag set by the operator
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{
		IMAP: IMAP{
			Port:          993,
			SubjectFilter: "PBX",
			PollInterval:  30 * time.Second,
		},
		Transcription: Transcription{
			Enabled:  true,
			Language: "nl-NL",
		},
		SSH: SSH{Port: 22},
		Cleanup: Cleanup{
			Schedule: "0 9 * * 5",
			Timezone: "Europe/Amsterdam",
		},
		Web: Web{
			Enabled: true,
			Listen:  ":8080",
		},
		Storage: Storage{
			Database: "data/voicemail.db",
			AudioDir: "data/audio",
		},
	}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	// Expand ${ENV_VAR} references after parsing, so secret values containing
	// YAML-special characters (@, #, *, ...) can never break the parse.
	expandEnvStrings(reflect.ValueOf(cfg).Elem())

	if cfg.IMAP.Server == "" || cfg.IMAP.Email == "" || cfg.IMAP.Password == "" {
		return nil, fmt.Errorf("imap server, email and password are required")
	}
	if cfg.Telegram.Token == "" || cfg.Telegram.ChatID == 0 {
		return nil, fmt.Errorf("telegram token and chat_id are required")
	}
	if cfg.IMAP.PollInterval < 5*time.Second {
		cfg.IMAP.PollInterval = 5 * time.Second
	}
	// Fail closed: a dashboard on a public https URL must not silently run
	// without authentication (e.g. because the GOOGLE_* env vars came
	// through empty).
	if cfg.Web.Enabled && strings.HasPrefix(cfg.Web.PublicURL, "https://") &&
		cfg.Web.GoogleAuth.ClientID == "" && cfg.Web.Password == "" &&
		!cfg.Web.AllowUnauthenticated {
		return nil, fmt.Errorf("web.public_url is public (https) but no authentication is configured — set web.google_auth (check that GOOGLE_CLIENT_ID/GOOGLE_CLIENT_SECRET are non-empty), or web.password, or explicitly set web.allow_unauthenticated: true")
	}
	if ga := cfg.Web.GoogleAuth; ga.ClientID != "" {
		if ga.ClientSecret == "" {
			return nil, fmt.Errorf("web.google_auth.client_secret is required when client_id is set")
		}
		if len(ga.AllowedDomains) == 0 {
			return nil, fmt.Errorf("web.google_auth.allowed_domains must list at least one domain, otherwise nobody can log in")
		}
		if cfg.Web.PublicURL == "" {
			return nil, fmt.Errorf("web.public_url is required when google_auth is enabled (it forms the OAuth redirect URL)")
		}
	}
	return cfg, nil
}

func expandEnv(s string) string {
	return os.Expand(s, func(key string) string {
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		return "${" + key + "}"
	})
}

// expandEnvStrings walks the config and expands ${ENV_VAR} references in
// every string value (struct fields, map values, slice elements).
func expandEnvStrings(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		if v.CanSet() {
			v.SetString(expandEnv(v.String()))
		}
	case reflect.Pointer:
		if !v.IsNil() {
			expandEnvStrings(v.Elem())
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			expandEnvStrings(v.Field(i))
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			// Map values are not addressable; expand on a copy and store it back.
			elem := reflect.New(v.Type().Elem()).Elem()
			elem.Set(v.MapIndex(key))
			expandEnvStrings(elem)
			v.SetMapIndex(key, elem)
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			expandEnvStrings(v.Index(i))
		}
	}
}
