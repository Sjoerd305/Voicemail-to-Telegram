// Command preview serves the web dashboard with seeded sample data, for
// working on the frontend without live IMAP/Telegram/PBX connections.
//
//	go run ./cmd/preview   # then open http://127.0.0.1:8099
package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/actions"
	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/config"
	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/mailer"
	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/store"
	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/web"
)

var transcripts = []string{
	"Goedemiddag, u spreekt met Jansen van de receptie. De lift op de derde verdieping doet het niet meer, kunt u iemand sturen? Bedankt.",
	"Hallo, met De Vries. De telefoon van kamer 12 blijft overgaan maar we kunnen niet opnemen. Graag even terugbellen.",
	"Ja goedemorgen, met Bakker van technische dienst. De storing van gisteren is verholpen, u hoeft niet meer langs te komen.",
	"Met Visser. Er staat een alarm op het paneel in de kelder, code E14. Kunnen jullie daar naar kijken vandaag nog?",
	"Goedenavond, de verwarming in het kantoor slaat steeds af. Het is hier inmiddels behoorlijk koud. Graag met spoed.",
	"",
	"Hallo, ik bel over het ticket van vorige week, nummer 4521. Is daar al nieuws over? U kunt mij bereiken op dit nummer.",
}

func main() {
	dir, err := os.MkdirTemp("", "voicemail-preview")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	st, err := store.Open(filepath.Join(dir, "preview.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	audioPath := filepath.Join(dir, "sample.wav")
	if err := os.WriteFile(audioPath, sampleWAV(23), 0o600); err != nil {
		log.Fatal(err)
	}

	rng := rand.New(rand.NewSource(42)) // #nosec G404 -- deterministic sample data, nothing security-sensitive
	now := time.Now()
	n := 0
	for day := 13; day >= 0; day-- {
		for i := 0; i < rng.Intn(4)+map[bool]int{true: 1, false: 0}[day%3 != 1]; i++ {
			at := now.AddDate(0, 0, -day).Add(-time.Duration(rng.Intn(600)) * time.Minute)
			n++
			vm := &store.Voicemail{
				ReceivedAt: at,
				Subject:    fmt.Sprintf("PBX Voicemail van 06%08d", 10000000+rng.Intn(89999999)),
				EmailText: func() string {
					num := fmt.Sprintf("06%08d", 10000000+rng.Intn(89999999))
					return fmt.Sprintf("Nieuw voicemailbericht in mailbox 9001.\nKlantnaam: %s\nFrom:    %q <%s>\nDuur: 0:%02d",
						[]string{"Bakkerij De Molen", "Zorgcentrum Avondrood", "Garage Peters BV", "Hotel Zeezicht", "Tandartspraktijk West"}[rng.Intn(5)],
						num, num, 10+rng.Intn(49))
				}(),
				Transcription: transcripts[rng.Intn(len(transcripts))],
				AudioPath:     audioPath,
				MessageID:     fmt.Sprintf("<preview-%d@example.com>", n),
			}
			if err := st.SaveVoicemail(vm); err != nil {
				log.Fatal(err)
			}
			// Mark most older voicemails handled so the done-state UI shows.
			if day > 1 && rng.Intn(10) < 8 {
				if _, err := st.SetDone(vm.ID, true); err != nil {
					log.Fatal(err)
				}
			}
		}
	}
	st.LogEvent("startup", "voicemailbot started")
	st.LogEvent("voicemail", "PBX Voicemail van 0612345678")
	st.LogEvent("command", "/vivia by sjoerd: Storingsdienst naar Vivia")
	st.LogEvent("cleanup", "inbox archived")
	st.LogEvent("error", "mail uid 42: transcription failed: context deadline exceeded")

	cfg := &config.Config{
		Commands: map[string]config.Command{
			"deletevm": {Type: "ssh", Command: "true", Success: "Voicemail verwijderd.", Primary: true},
			"vivia":    {Type: "ssh", Command: "true", Success: "Storingsdienst naar Vivia", Group: "storingsdienst"},
			"avics":    {Type: "ssh", Command: "true", Success: "Storingsdienst naar Avics", Group: "storingsdienst"},
		},
		PhoneNumbers: map[string]string{
			"jan": "0612345678", "piet": "0687654321", "kees": "0611111111",
			"anna": "0622222222", "bram": "0633333333", "daan": "0644444444",
			"eva": "0655555555",
		},
	}
	watcher := mailer.NewWatcher(cfg, st, nil, nil)
	srv := web.NewServer(cfg, st, actions.New(cfg), watcher, nil)

	// Fake a healthy recent poll for the header pill.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		count, _ := st.CountVoicemails()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"uptime_seconds":86400,"voicemail_count":%d,"watcher":{"last_poll":%q,"last_error":""}}`,
			count, time.Now().Add(-20*time.Second).Format(time.RFC3339))
	})
	mux.Handle("/", srv.Handler())

	fmt.Println("preview on http://127.0.0.1:8099 with", n, "sample voicemails")
	previewSrv := &http.Server{
		Addr:              "127.0.0.1:8099",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Fatal(previewSrv.ListenAndServe())
}

// sampleWAV generates a wobbling tone, seconds long, 8kHz 16-bit mono.
func sampleWAV(seconds int) []byte {
	const rate = 8000
	samples := rate * seconds
	data := make([]byte, 44+samples*2)
	copy(data[0:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(36+samples*2)) // #nosec G115 -- samples is rate*seconds, far below uint32 max
	copy(data[8:12], "WAVE")
	copy(data[12:16], "fmt ")
	binary.LittleEndian.PutUint32(data[16:20], 16)
	binary.LittleEndian.PutUint16(data[20:22], 1)
	binary.LittleEndian.PutUint16(data[22:24], 1)
	binary.LittleEndian.PutUint32(data[24:28], rate)
	binary.LittleEndian.PutUint32(data[28:32], rate*2)
	binary.LittleEndian.PutUint16(data[32:34], 2)
	binary.LittleEndian.PutUint16(data[34:36], 16)
	copy(data[36:40], "data")
	binary.LittleEndian.PutUint32(data[40:44], uint32(samples*2)) // #nosec G115 -- samples is rate*seconds, far below uint32 max
	for i := 0; i < samples; i++ {
		t := float64(i) / rate
		v := math.Sin(2*math.Pi*220*t) * math.Sin(2*math.Pi*0.8*t) * 0.3
		binary.LittleEndian.PutUint16(data[44+i*2:], uint16(int16(v*32767))) // #nosec G115 -- two's-complement bit pattern is exactly what PCM wants
	}
	return data
}
