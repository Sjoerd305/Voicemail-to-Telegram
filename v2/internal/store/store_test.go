package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestListVoicemailsPaging(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	for i := 1; i <= 25; i++ {
		vm := &Voicemail{ReceivedAt: now.Add(-time.Duration(i) * time.Hour), Subject: "vm", Transcription: "storing bij klant"}
		if i%5 == 0 {
			vm.Transcription = "100% zeker_weten"
		}
		if err := s.SaveVoicemail(vm); err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			if _, err := s.SetDone(vm.ID, true); err != nil {
				t.Fatal(err)
			}
		}
	}

	page, err := s.ListVoicemails(ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 10 || page.Total != 25 || !page.HasMore {
		t.Fatalf("first page: got %d items, total %d, has_more %v", len(page.Items), page.Total, page.HasMore)
	}
	if page.Items[0].ID != 25 || page.Items[9].ID != 16 {
		t.Fatalf("first page ids %d..%d, want 25..16", page.Items[0].ID, page.Items[9].ID)
	}

	page, err = s.ListVoicemails(ListOptions{Limit: 10, Before: page.Items[9].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 10 || page.Items[0].ID != 15 || !page.HasMore {
		t.Fatalf("second page: %d items starting at %d, has_more %v", len(page.Items), page.Items[0].ID, page.HasMore)
	}

	page, err = s.ListVoicemails(ListOptions{Limit: 10, Before: page.Items[9].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 5 || page.HasMore {
		t.Fatalf("last page: %d items, has_more %v", len(page.Items), page.HasMore)
	}

	open := false
	page, err = s.ListVoicemails(ListOptions{Limit: 100, Done: &open})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 13 || len(page.Items) != 13 {
		t.Fatalf("open filter: total %d, %d items", page.Total, len(page.Items))
	}

	// LIKE wildcards in the query must be treated literally.
	page, err = s.ListVoicemails(ListOptions{Query: "100% ZEKER_weten"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 5 {
		t.Fatalf("query total %d, want 5", page.Total)
	}
	page, err = s.ListVoicemails(ListOptions{Query: "100%_zeker"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 0 {
		t.Fatalf("escaped query total %d, want 0", page.Total)
	}
}

func TestStats(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 8, 19, 15, 0, 0, 0, time.Local) // Wednesday
	for _, age := range []time.Duration{time.Hour, 30 * time.Hour, 4 * 24 * time.Hour, 20 * 24 * time.Hour} {
		vm := &Voicemail{ReceivedAt: now.Add(-age)}
		if err := s.SaveVoicemail(vm); err != nil {
			t.Fatal(err)
		}
	}
	st, err := s.Stats(now)
	if err != nil {
		t.Fatal(err)
	}
	if st.Open != 4 || st.Today != 1 || st.Week != 2 {
		t.Fatalf("open %d today %d week %d", st.Open, st.Today, st.Week)
	}
	if len(st.Days) != 14 || st.Days[13].Date != "2026-08-19" || st.Days[13].Count != 1 || st.Days[12].Count != 1 || st.Days[9].Count != 1 {
		t.Fatalf("days: %+v", st.Days)
	}
}

func TestListVoicemailsPeriod(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	for _, age := range []time.Duration{time.Hour, 48 * time.Hour, 10 * 24 * time.Hour} {
		if err := s.SaveVoicemail(&Voicemail{ReceivedAt: now.Add(-age)}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.ListVoicemails(ListOptions{Since: now.Add(-3 * 24 * time.Hour), Until: now.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != 2 {
		t.Fatalf("period: total %d items %+v", page.Total, page.Items)
	}
}

func TestPruneAudio(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	now := time.Now()
	seq := 0
	mk := func(age time.Duration, withFile bool) *Voicemail {
		seq++
		vm := &Voicemail{ReceivedAt: now.Add(-age), Transcription: "tekst blijft"}
		if withFile {
			vm.AudioPath = filepath.Join(dir, fmt.Sprintf("%d.wav", seq))
			if err := os.WriteFile(vm.AudioPath, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.SaveVoicemail(vm); err != nil {
			t.Fatal(err)
		}
		return vm
	}
	old := mk(100*24*time.Hour, true)
	gone := mk(95*24*time.Hour, true)
	os.Remove(gone.AudioPath) // already missing on disk
	recent := mk(10*24*time.Hour, true)
	mk(200*24*time.Hour, false) // no audio to begin with

	n, err := s.PruneAudio(now.AddDate(0, 0, -90))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("pruned %d, want 2", n)
	}
	if _, err := os.Stat(old.AudioPath); !os.IsNotExist(err) {
		t.Fatalf("old audio still exists: %v", err)
	}
	if _, err := os.Stat(recent.AudioPath); err != nil {
		t.Fatalf("recent audio removed: %v", err)
	}
	for _, id := range []int64{old.ID, gone.ID} {
		vm, err := s.GetVoicemail(id)
		if err != nil {
			t.Fatal(err)
		}
		if vm.HasAudio || vm.Transcription != "tekst blijft" {
			t.Fatalf("vm %d after prune: has_audio %v transcription %q", id, vm.HasAudio, vm.Transcription)
		}
	}
	if vm, _ := s.GetVoicemail(recent.ID); !vm.HasAudio {
		t.Fatal("recent voicemail lost its audio")
	}
	// Idempotent.
	if n, err := s.PruneAudio(now.AddDate(0, 0, -90)); err != nil || n != 0 {
		t.Fatalf("second prune: n=%d err=%v", n, err)
	}
}
