// Package bot runs the Telegram side: it delivers voicemail notifications
// and handles the group commands (/deletevm, /vivia, storingsdienst
// switches, customer lookups, ...).
package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Sjoerd305/Voicemail-to-Telegram/internal/actions"
	"github.com/Sjoerd305/Voicemail-to-Telegram/internal/config"
	"github.com/Sjoerd305/Voicemail-to-Telegram/internal/store"
)

type Bot struct {
	cfg    *config.Config
	api    *tgbotapi.BotAPI
	runner *actions.Runner
	store  *store.Store
}

func New(cfg *config.Config, runner *actions.Runner, st *store.Store) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(cfg.Telegram.Token)
	if err != nil {
		return nil, fmt.Errorf("telegram: %w", err)
	}
	slog.Info("telegram bot authorized", "username", api.Self.UserName)
	return &Bot{cfg: cfg, api: api, runner: runner, store: st}, nil
}

// SendVoicemail sends the voicemail audio with its caption, plus any
// overflow text as follow-up messages. Retries with backoff on network
// errors; Telegram rate limits are respected via the returned retry-after.
func (b *Bot) SendVoicemail(caption string, extraParts []string, audio []byte) error {
	if len(audio) > 0 {
		voice := tgbotapi.NewVoice(b.cfg.Telegram.ChatID, tgbotapi.FileBytes{
			Name:  "voicemail.wav",
			Bytes: audio,
		})
		voice.Caption = caption
		if err := b.sendWithRetry(voice); err != nil {
			return err
		}
	} else if err := b.sendWithRetry(tgbotapi.NewMessage(b.cfg.Telegram.ChatID, caption)); err != nil {
		return err
	}
	for _, part := range extraParts {
		if err := b.sendWithRetry(tgbotapi.NewMessage(b.cfg.Telegram.ChatID, part)); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bot) sendWithRetry(c tgbotapi.Chattable) error {
	var lastErr error
	for attempt := 1; attempt <= 5; attempt++ {
		_, err := b.api.Send(c)
		if err == nil {
			return nil
		}
		lastErr = err
		wait := time.Duration(attempt*attempt) * time.Second
		if tgErr, ok := err.(*tgbotapi.Error); ok && tgErr.RetryAfter > 0 {
			wait = time.Duration(tgErr.RetryAfter) * time.Second
		}
		slog.Warn("telegram send failed, retrying", "attempt", attempt, "wait", wait, "err", err)
		time.Sleep(wait)
	}
	return lastErr
}

// SendMessage sends a plain text message to the configured group chat. Used
// by the web UI to announce actions there, so the group stays the single
// source of truth for what happened.
func (b *Bot) SendMessage(text string) error {
	return b.sendWithRetry(tgbotapi.NewMessage(b.cfg.Telegram.ChatID, text))
}

// Run consumes updates until the context is cancelled.
func (b *Bot) Run(ctx context.Context) {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	updates := b.api.GetUpdatesChan(u)
	b.registerCommands()
	slog.Info("telegram command listener started")
	for {
		select {
		case <-ctx.Done():
			b.api.StopReceivingUpdates()
			return
		case update := <-updates:
			if update.Message == nil || !update.Message.IsCommand() {
				continue
			}
			b.handleCommand(ctx, update.Message)
		}
	}
}

func (b *Bot) handleCommand(ctx context.Context, msg *tgbotapi.Message) {
	// Only react in the configured group chat, so commands cannot be issued
	// from random private chats.
	if msg.Chat.ID != b.cfg.Telegram.ChatID {
		slog.Warn("ignoring command from unknown chat", "chat_id", msg.Chat.ID, "command", msg.Command())
		return
	}
	cmd := strings.ToLower(msg.Command())
	sender := senderName(msg.From)
	slog.Info("received command", "command", cmd, "from", sender)

	switch cmd {
	case "info":
		b.reply(msg, b.infoMessage())
		return
	case "storingsdienst", "nummers":
		b.reply(msg, b.storingsdienstHelp())
		return
	case "lol":
		b.reply(msg, "https://www.youtube.com/watch?v=0q6yphdZhUA")
		return
	}

	if result, _, found := b.runner.Run(ctx, cmd); found {
		b.store.LogEvent("command", fmt.Sprintf("/%s by %s: %s", cmd, sender, result))
		b.reply(msg, result)
		return
	}

	// Fall back to customer lookup, like the old bot.
	if info, ok := b.lookupCustomer(cmd); ok {
		b.reply(msg, info)
		return
	}
	b.reply(msg, "Onbekend commando, zie /info")
}

// Telegram's limits for registered bot commands.
const (
	maxMenuCommands   = 100
	maxDescriptionLen = 256
)

var validCommandName = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

// registerCommands publishes the command menu (the "/" button in Telegram)
// for the group chat, so the list stays in step with the configuration.
// Failure is logged only: a menu is a convenience, not a reason to refuse
// to start.
func (b *Bot) registerCommands() {
	cmds := b.menuCommands()
	scope := tgbotapi.NewBotCommandScopeChat(b.cfg.Telegram.ChatID)
	if _, err := b.api.Request(tgbotapi.NewSetMyCommandsWithScope(scope, cmds...)); err != nil {
		slog.Error("failed to register telegram command menu", "err", err)
		return
	}
	slog.Info("telegram command menu registered", "count", len(cmds))
}

// menuCommands builds the command menu from the same sources as /info:
// the fixed commands, the configured actions and the storingsdienst
// switches. Customer lookups are left out on purpose; that file changes
// without a restart and could push the list past Telegram's limit.
func (b *Bot) menuCommands() []tgbotapi.BotCommand {
	cmds := []tgbotapi.BotCommand{
		{Command: "info", Description: "Toon alle commando's"},
		{Command: "storingsdienst", Description: "Storingsdienst-commando's"},
	}

	names := make([]string, 0, len(b.cfg.Commands))
	for name := range b.cfg.Commands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cmd := b.cfg.Commands[name]
		desc := cmd.Description
		if desc == "" {
			desc = cmd.Success
		}
		cmds = append(cmds, tgbotapi.BotCommand{Command: name, Description: desc})
	}

	keys := make([]string, 0, len(b.cfg.PhoneNumbers))
	for key := range b.cfg.PhoneNumbers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		cmds = append(cmds, tgbotapi.BotCommand{
			Command:     actions.Slugify(key),
			Description: "Storingsdienst naar " + key,
		})
	}

	// Drop anything Telegram would reject, rather than losing the whole menu.
	valid := cmds[:0]
	seen := make(map[string]bool)
	for _, c := range cmds {
		if !validCommandName.MatchString(c.Command) || seen[c.Command] {
			slog.Warn("skipping command in telegram menu", "command", c.Command)
			continue
		}
		seen[c.Command] = true
		c.Description = menuDescription(c.Description, c.Command)
		valid = append(valid, c)
		if len(valid) == maxMenuCommands {
			break
		}
	}
	return valid
}

// menuDescription keeps a description within Telegram's 3-256 characters.
func menuDescription(desc, fallback string) string {
	desc = strings.TrimSpace(desc)
	if len(desc) < 3 {
		desc = "/" + fallback
	}
	if len(desc) > maxDescriptionLen {
		desc = string([]rune(desc)[:maxDescriptionLen-1]) + "…"
	}
	return desc
}

// senderName is who to credit for a command. A Telegram @username is
// optional, so relying on it alone left the event log reading "by "; the
// first/last name is always present and matches the real names the web UI
// gets from Google.
func senderName(u *tgbotapi.User) string {
	if u == nil {
		return "onbekend"
	}
	if name := strings.TrimSpace(u.FirstName + " " + u.LastName); name != "" {
		return name
	}
	if u.UserName != "" {
		return "@" + u.UserName
	}
	return "onbekend"
}

// phoneCommandList lists the generated command for every configured phone
// number, since full names in the config no longer map 1:1 to a command.
func (b *Bot) phoneCommandList() string {
	keys := make([]string, 0, len(b.cfg.PhoneNumbers))
	for key := range b.cfg.PhoneNumbers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&sb, "/%s – %s\n", actions.Slugify(key), key)
	}
	sb.WriteString("Een voornaam werkt ook als die eenduidig is, bv. /sjoerd.")
	return sb.String()
}

func (b *Bot) storingsdienstHelp() string {
	return "Storingsdienst omzetten:\n" + b.phoneCommandList()
}

// infoMessage builds the /info reply dynamically from the configuration, so
// the command list can never go stale. An optional info_file is appended for
// free-form notes.
func (b *Bot) infoMessage() string {
	var sb strings.Builder
	sb.WriteString("Beschikbare commando's:\n\n")
	sb.WriteString("/info – Toon dit bericht\n")

	names := make([]string, 0, len(b.cfg.Commands))
	for name := range b.cfg.Commands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cmd := b.cfg.Commands[name]
		desc := cmd.Description
		if desc == "" {
			desc = cmd.Success
		}
		fmt.Fprintf(&sb, "/%s – %s\n", name, desc)
	}

	if len(b.cfg.PhoneNumbers) > 0 {
		sb.WriteString("\nStoringsdienst omzetten:\n")
		sb.WriteString(b.phoneCommandList())
		sb.WriteString("\n")
	}

	if customers := b.customerCommands(); len(customers) > 0 {
		sb.WriteString("\nKlantinfo: /")
		sb.WriteString(strings.Join(customers, ", /"))
		sb.WriteString("\n")
	}

	if b.cfg.InfoFile != "" {
		if raw, err := os.ReadFile(b.cfg.InfoFile); err == nil {
			if extra := strings.TrimSpace(string(raw)); extra != "" {
				sb.WriteString("\n")
				sb.WriteString(extra)
			}
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func (b *Bot) customerCommands() []string {
	if b.cfg.CustomersFile == "" {
		return nil
	}
	raw, err := os.ReadFile(b.cfg.CustomersFile)
	if err != nil {
		return nil
	}
	var customers map[string]string
	if err := json.Unmarshal(raw, &customers); err != nil {
		return nil
	}
	keys := make([]string, 0, len(customers))
	for key := range customers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (b *Bot) lookupCustomer(cmd string) (string, bool) {
	if b.cfg.CustomersFile == "" {
		return "", false
	}
	// Read on every request so the file can be edited without a restart.
	raw, err := os.ReadFile(b.cfg.CustomersFile)
	if err != nil {
		slog.Error("failed to read customers file", "err", err)
		return "", false
	}
	var customers map[string]string
	if err := json.Unmarshal(raw, &customers); err != nil {
		slog.Error("failed to parse customers file", "err", err)
		return "", false
	}
	info, ok := customers[cmd]
	return info, ok
}

func (b *Bot) reply(msg *tgbotapi.Message, text string) {
	m := tgbotapi.NewMessage(msg.Chat.ID, text)
	m.ReplyToMessageID = msg.MessageID
	if _, err := b.api.Send(m); err != nil {
		slog.Error("failed to send reply", "err", err)
	}
}
