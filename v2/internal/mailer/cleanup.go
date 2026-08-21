package mailer

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/config"
	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/store"
)

// Cleanup archives everything in INBOX into a per-week folder, e.g.
// "INBOX.2026.33-34" — same naming as the old emailcleanup.py.
type Cleanup struct {
	cfg   *config.Config
	store *store.Store
}

func NewCleanup(cfg *config.Config, st *store.Store) *Cleanup {
	return &Cleanup{cfg: cfg, store: st}
}

func folderName(now time.Time) string {
	year, week := now.ISOWeek()
	return fmt.Sprintf("INBOX.%d.%d-%d", year, week-1, week)
}

func (c *Cleanup) Run() {
	if err := c.run(); err != nil {
		slog.Error("email cleanup failed", "err", err)
		c.store.LogEvent("error", "email cleanup: "+err.Error())
		return
	}
	c.store.LogEvent("cleanup", "inbox archived")
}

func (c *Cleanup) run() error {
	addr := fmt.Sprintf("%s:%d", c.cfg.IMAP.Server, c.cfg.IMAP.Port)
	client, err := imapclient.DialTLS(addr, nil)
	if err != nil {
		return fmt.Errorf("dial imap: %w", err)
	}
	defer client.Close()

	if err := client.Login(c.cfg.IMAP.Email, c.cfg.IMAP.Password).Wait(); err != nil {
		return fmt.Errorf("imap login: %w", err)
	}
	defer client.Logout()

	folder := folderName(time.Now())

	// Create the folder if it does not exist yet.
	if err := client.Create(folder, nil).Wait(); err != nil {
		// Most servers return an error if it already exists; that is fine.
		slog.Debug("create folder", "folder", folder, "err", err)
	}

	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		return fmt.Errorf("select inbox: %w", err)
	}

	searchData, err := client.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	if err != nil {
		return fmt.Errorf("imap search: %w", err)
	}
	uids := searchData.AllUIDs()
	if len(uids) == 0 {
		slog.Info("email cleanup: inbox empty, nothing to archive")
		return nil
	}

	uidSet := imap.UIDSetNum(uids...)
	if _, err := client.Copy(uidSet, folder).Wait(); err != nil {
		return fmt.Errorf("copy to %s: %w", folder, err)
	}
	if err := client.Store(uidSet, &imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Flags:  []imap.Flag{imap.FlagDeleted},
		Silent: true,
	}, nil).Close(); err != nil {
		return fmt.Errorf("mark deleted: %w", err)
	}
	if err := client.Expunge().Close(); err != nil {
		return fmt.Errorf("expunge: %w", err)
	}
	slog.Info("email cleanup done", "folder", folder, "moved", len(uids))
	return nil
}
