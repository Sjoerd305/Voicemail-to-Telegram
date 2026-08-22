// Package web serves the JSON API and the embedded TypeScript frontend.
package web

import (
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/actions"
	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/config"
	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/mailer"
	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/store"
)

//go:embed all:dist
var distFS embed.FS

// Notifier posts a message to the Telegram group; nil disables notifying.
type Notifier interface {
	SendMessage(text string) error
}

type Server struct {
	cfg      *config.Config
	store    *store.Store
	runner   *actions.Runner
	watcher  *mailer.Watcher
	notifier Notifier
	started  time.Time
}

func NewServer(cfg *config.Config, st *store.Store, runner *actions.Runner, watcher *mailer.Watcher, notifier Notifier) *Server {
	return &Server{cfg: cfg, store: st, runner: runner, watcher: watcher, notifier: notifier, started: time.Now()}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/voicemails", s.handleListVoicemails)
	mux.HandleFunc("GET /api/voicemails/{id}/audio", s.handleAudio)
	mux.HandleFunc("POST /api/voicemails/{id}/done", s.handleSetDone)
	mux.HandleFunc("GET /api/events", s.handleListEvents)
	mux.HandleFunc("GET /api/actions", s.handleListActions)
	mux.HandleFunc("POST /api/actions/{name}", s.handleRunAction)

	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err)
	}
	static := http.FileServerFS(sub)
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// no-cache = revalidate before reuse; keeps CDN/proxy edges (e.g.
		// Cloudflare caches .js/.css by extension) from serving stale
		// frontend builds after a deploy.
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	}))

	if s.cfg.Web.GoogleAuth.ClientID != "" {
		return newGoogleAuth(s.cfg.Web).middleware(mux)
	}
	return s.basicAuth(mux)
}

func (s *Server) basicAuth(next http.Handler) http.Handler {
	password := s.cfg.Web.Password
	if password == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="voicemail"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("failed to encode response", "err", err)
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	count, _ := s.store.CountVoicemails()
	writeJSON(w, map[string]any{
		"uptime_seconds":  int(time.Since(s.started).Seconds()),
		"voicemail_count": count,
		"watcher":         s.watcher.Status(),
		"auth_enabled":    s.cfg.Web.GoogleAuth.ClientID != "",
	})
}

func (s *Server) handleListVoicemails(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	vms, err := s.store.ListVoicemails(limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, vms)
}

func (s *Server) handleAudio(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	vm, err := s.store.GetVoicemail(id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && vm.AudioPath == "") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, err := os.Open(vm.AudioPath)
	if err != nil {
		http.Error(w, "audio file missing", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "audio/wav")
	http.ServeContent(w, r, "voicemail.wav", vm.ReceivedAt, f)
}

func (s *Server) handleSetDone(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var body struct {
		Done bool `json:"done"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	vm, err := s.store.SetDone(id, body.Done)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	what := "heropend"
	if body.Done {
		what = "afgehandeld"
	}
	if actor, ok := userFromRequest(r); ok {
		s.store.LogEvent("done", fmt.Sprintf("voicemail #%d %s door %s", id, what, actor.DisplayName()))
	} else {
		s.store.LogEvent("done", fmt.Sprintf("voicemail #%d %s", id, what))
	}
	writeJSON(w, vm)
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.store.ListEvents(limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, events)
}

func (s *Server) handleListActions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.runner.List())
}

func (s *Server) handleRunAction(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	msg, ok, found := s.runner.Run(r.Context(), name)
	if !found {
		http.Error(w, "unknown action", http.StatusNotFound)
		return
	}
	// Attribute the action to the signed-in Google account, matching the
	// "/cmd by <name>" lines the Telegram side writes.
	via := " (via web-UI)"
	if actor, ok := userFromRequest(r); ok {
		s.store.LogEvent("command", fmt.Sprintf("/%s by %s (web): %s", name, actor.DisplayName(), msg))
		via = " (via web-UI door " + actor.DisplayName() + ")"
	} else {
		s.store.LogEvent("command", fmt.Sprintf("/%s via web-UI: %s", name, msg))
	}
	// Announce the result in the Telegram group so web actions are just as
	// visible to the team as chat commands. Async: Telegram latency or
	// retries should not delay the HTTP response.
	if s.notifier != nil {
		go func() {
			if err := s.notifier.SendMessage(msg + via); err != nil {
				slog.Error("failed to announce web action in telegram", "action", name, "err", err)
			}
		}()
	}
	writeJSON(w, map[string]any{"ok": ok, "message": msg})
}
