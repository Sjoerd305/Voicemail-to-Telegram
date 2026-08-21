// Package actions executes the configured PBX commands (over SSH or HTTP).
// It is shared by the Telegram bot and the web API so both expose the exact
// same set of operations.
package actions

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/config"
)

type Runner struct {
	cfg *config.Config
}

func New(cfg *config.Config) *Runner {
	return &Runner{cfg: cfg}
}

// Action is something that can be triggered by name: either a configured
// command (ssh/http) or a storingsdienst phone-number switch.
type Action struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"` // "command" or "storingsdienst"
	Primary bool   `json:"primary"`
	Group   string `json:"group"`
}

func (r *Runner) List() []Action {
	var out []Action
	for name, cmd := range r.cfg.Commands {
		out = append(out, Action{Name: name, Kind: "command", Primary: cmd.Primary, Group: cmd.Group})
	}
	for name := range r.cfg.PhoneNumbers {
		out = append(out, Action{Name: name, Kind: "storingsdienst", Group: "storingsdienst"})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Primary != out[j].Primary {
			return out[i].Primary
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Run executes the named action and returns the user-facing result message.
// ok is false when the action failed; found is false when no action with
// this name exists.
func (r *Runner) Run(ctx context.Context, name string) (msg string, ok, found bool) {
	if cmd, exists := r.cfg.Commands[name]; exists {
		msg, ok = r.runCommand(ctx, name, cmd)
		return msg, ok, true
	}
	if canonical, number, exists := r.resolvePhone(name); exists {
		msg, ok = r.setStoringsdienst(ctx, canonical, number)
		return msg, ok, true
	}
	return "", false, false
}

// Slugify turns a phone_numbers key into a Telegram-safe command name:
// "Sjoerd van Dijk (prive)" -> "sjoerd_van_dijk_prive". Telegram commands
// may only contain lowercase letters, digits and underscores.
func Slugify(name string) string {
	var b strings.Builder
	prevSep := true
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			prevSep = false
		default:
			if !prevSep {
				b.WriteByte('_')
				prevSep = true
			}
		}
	}
	return strings.TrimSuffix(b.String(), "_")
}

// resolvePhone maps an action name to a phone_numbers entry. It accepts the
// exact key (used by the web UI), its slug (used as Telegram command), or a
// first-name shortcut like /sjoerd when that unambiguously points to one
// entry — entries with a "(...)" suffix (e.g. prive numbers) don't count
// against the shortcut, matching how the old first-name commands behaved.
func (r *Runner) resolvePhone(name string) (canonical, number string, found bool) {
	if num, exists := r.cfg.PhoneNumbers[name]; exists {
		return name, num, true
	}
	slug := Slugify(name)
	if slug == "" {
		return "", "", false
	}
	var exact, prefix, prefixPlain []string
	for key := range r.cfg.PhoneNumbers {
		keySlug := Slugify(key)
		switch {
		case keySlug == slug:
			exact = append(exact, key)
		case strings.HasPrefix(keySlug, slug+"_"):
			prefix = append(prefix, key)
			if !strings.Contains(key, "(") {
				prefixPlain = append(prefixPlain, key)
			}
		}
	}
	for _, candidates := range [][]string{exact, prefix, prefixPlain} {
		if len(candidates) == 1 {
			return candidates[0], r.cfg.PhoneNumbers[candidates[0]], true
		}
	}
	return "", "", false
}

func (r *Runner) runCommand(ctx context.Context, name string, cmd config.Command) (string, bool) {
	var err error
	switch cmd.Type {
	case "ssh":
		err = r.runSSH(ctx, cmd.Command)
	case "http":
		err = r.runHTTP(ctx, cmd.URL)
	default:
		err = fmt.Errorf("unknown command type %q", cmd.Type)
	}
	if err != nil {
		slog.Error("command failed", "command", name, "err", err)
		if cmd.Error != "" {
			return cmd.Error, false
		}
		return "Er is een fout opgetreden.", false
	}
	slog.Info("command executed", "command", name)
	return cmd.Success, true
}

func (r *Runner) setStoringsdienst(ctx context.Context, name, number string) (string, bool) {
	u := fmt.Sprintf("http://%s/storingsdienst/setnummer.php?setnummer=%s",
		r.cfg.PBXIP, url.QueryEscape(number))
	if err := r.runHTTP(ctx, u); err != nil {
		slog.Error("storingsdienst switch failed", "name", name, "err", err)
		return "Er is een fout opgetreden.", false
	}
	slog.Info("storingsdienst switched", "name", name)
	return "Storingsdienst naar " + capitalize(name), true
}

func (r *Runner) runHTTP(ctx context.Context, rawURL string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

func (r *Runner) runSSH(ctx context.Context, command string) error {
	cfg := &ssh.ClientConfig{
		User:            r.cfg.SSH.Username,
		Auth:            []ssh.AuthMethod{ssh.Password(r.cfg.SSH.Password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // #nosec G106 -- same trust model as the old paramiko AutoAddPolicy; PBX host lives on the trusted LAN
		Timeout:         10 * time.Second,
	}
	addr := net.JoinHostPort(r.cfg.SSH.Host, strconv.Itoa(r.cfg.SSH.Port))

	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = func() error {
			client, err := ssh.Dial("tcp", addr, cfg)
			if err != nil {
				return fmt.Errorf("dial: %w", err)
			}
			defer client.Close()
			sess, err := client.NewSession()
			if err != nil {
				return fmt.Errorf("session: %w", err)
			}
			defer sess.Close()
			out, err := sess.CombinedOutput(command)
			if err != nil {
				return fmt.Errorf("run %q: %w (output: %s)", command, err, out)
			}
			return nil
		}()
		if lastErr == nil {
			return nil
		}
		slog.Warn("ssh command attempt failed", "attempt", attempt, "err", lastErr)
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	return lastErr
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}
