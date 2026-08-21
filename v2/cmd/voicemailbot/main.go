package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/actions"
	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/bot"
	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/config"
	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/mailer"
	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/store"
	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/transcribe"
	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/web"
)

func main() {
	configPath := flag.String("config", "config/config.yaml", "path to config file")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	if err := run(*configPath); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(cfg.Storage.AudioDir, 0o750); err != nil {
		return fmt.Errorf("create audio dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Storage.Database), 0o750); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	// Single-instance guard: the old setup could end up with the bot running
	// multiple times, sending every voicemail more than once. An exclusive
	// flock on a lock file makes a second instance exit immediately.
	unlock, err := acquireLock(cfg.Storage.Database + ".lock")
	if err != nil {
		return err
	}
	defer unlock()

	st, err := store.Open(cfg.Storage.Database)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	transcriber, err := transcribe.New(ctx, cfg.Transcription)
	if err != nil {
		return err
	}
	defer transcriber.Close()

	runner := actions.New(cfg)

	tgBot, err := bot.New(cfg, runner, st)
	if err != nil {
		return err
	}

	watcher := mailer.NewWatcher(cfg, st, transcriber, tgBot)

	// Weekly inbox cleanup.
	var cronRunner *cron.Cron
	if cfg.Cleanup.Enabled {
		loc, err := time.LoadLocation(cfg.Cleanup.Timezone)
		if err != nil {
			return fmt.Errorf("cleanup timezone: %w", err)
		}
		cronRunner = cron.New(cron.WithLocation(loc))
		cleanup := mailer.NewCleanup(cfg, st)
		if _, err := cronRunner.AddFunc(cfg.Cleanup.Schedule, cleanup.Run); err != nil {
			return fmt.Errorf("cleanup schedule: %w", err)
		}
		cronRunner.Start()
		defer cronRunner.Stop()
		slog.Info("email cleanup scheduled", "cron", cfg.Cleanup.Schedule, "tz", cfg.Cleanup.Timezone)
	}

	go watcher.Run(ctx)
	go tgBot.Run(ctx)

	var srv *http.Server
	if cfg.Web.Enabled {
		// Make the effective auth mode unmissable in the logs.
		switch {
		case cfg.Web.GoogleAuth.ClientID != "":
			slog.Info("web auth: google sign-in enabled",
				"allowed_domains", cfg.Web.GoogleAuth.AllowedDomains)
		case cfg.Web.Password != "":
			slog.Info("web auth: basic auth enabled")
		default:
			slog.Warn("web auth: NONE — dashboard is accessible without login")
		}
		srv = &http.Server{
			Addr:              cfg.Web.Listen,
			Handler:           web.NewServer(cfg, st, runner, watcher, tgBot).Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			slog.Info("web server listening", "addr", cfg.Web.Listen)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("web server failed", "err", err)
			}
		}()
	}

	st.LogEvent("startup", "voicemailbot started")
	<-ctx.Done()
	slog.Info("shutting down")
	if srv != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("web server shutdown", "err", err)
		}
	}
	return nil
}

func acquireLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- path derives from the operator's config file
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another instance is already running (lock %s held)", path)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		_ = os.Remove(path)
	}, nil
}
