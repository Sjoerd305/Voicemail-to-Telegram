package transcribe

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"cloud.google.com/go/auth/credentials"
	speech "cloud.google.com/go/speech/apiv1"
	"cloud.google.com/go/speech/apiv1/speechpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/option"

	"github.com/Sjoerd305/Voicemail-to-Telegram/v2/internal/config"
)

type Transcriber struct {
	cfg     config.Transcription
	speech  *speech.Client
	storage *storage.Client
}

func New(ctx context.Context, cfg config.Transcription) (*Transcriber, error) {
	if !cfg.Enabled {
		return &Transcriber{cfg: cfg}, nil
	}
	// With an empty CredentialsFile this falls back to Application Default
	// Credentials (GOOGLE_APPLICATION_CREDENTIALS etc.). The credentials are
	// built once and shared by the speech and storage clients.
	creds, err := credentials.DetectDefault(&credentials.DetectOptions{
		CredentialsFile: cfg.CredentialsFile,
		Scopes:          []string{"https://www.googleapis.com/auth/cloud-platform"},
	})
	if err != nil {
		return nil, fmt.Errorf("google credentials: %w", err)
	}
	opts := []option.ClientOption{option.WithAuthCredentials(creds)}
	sc, err := speech.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("speech client: %w", err)
	}
	t := &Transcriber{cfg: cfg, speech: sc}
	if cfg.GCSBucket != "" {
		gcs, err := storage.NewClient(ctx, opts...)
		if err != nil {
			_ = sc.Close()
			return nil, fmt.Errorf("storage client: %w", err)
		}
		t.storage = gcs
	}
	return t, nil
}

func (t *Transcriber) Close() {
	if t.speech != nil {
		_ = t.speech.Close()
	}
	if t.storage != nil {
		_ = t.storage.Close()
	}
}

// Transcribe converts a WAV voicemail to text. When a GCS bucket is
// configured, all audio is routed through it (the object is deleted again
// afterwards); without a bucket, audio is sent inline, which Google limits
// to ~60 seconds. No more splitting into segments.
func (t *Transcriber) Transcribe(ctx context.Context, wav []byte) (string, error) {
	if !t.cfg.Enabled || t.speech == nil {
		return "", nil
	}

	dur, sampleRate := probeWAV(wav)
	slog.Info("transcribing voicemail", "duration", dur.Round(time.Second), "sample_rate", sampleRate)

	recCfg := &speechpb.RecognitionConfig{
		// For WAV input the encoding and sample rate are read from the file
		// header; we still pass the sample rate as a fallback.
		Encoding:        speechpb.RecognitionConfig_LINEAR16,
		SampleRateHertz: sampleRate,
		LanguageCode:    t.cfg.Language,
	}

	// One route per deployment: with a bucket configured, all audio goes
	// through GCS so that path is exercised constantly and can't rot;
	// without one, everything is sent inline (limited to ~60s by Google).
	// Deliberately no inline fallback when the bucket fails — that would
	// silently mask a broken bucket configuration.
	if t.storage == nil {
		resp, err := t.speech.Recognize(ctx, &speechpb.RecognizeRequest{
			Config: recCfg,
			Audio: &speechpb.RecognitionAudio{
				AudioSource: &speechpb.RecognitionAudio_Content{Content: wav},
			},
		})
		if err != nil {
			return "", fmt.Errorf("inline recognize (audio %s, no gcs_bucket configured): %w",
				dur.Round(time.Second), err)
		}
		return joinResults(resp.Results), nil
	}

	objName := fmt.Sprintf("voicemail-%d.wav", time.Now().UnixNano())
	obj := t.storage.Bucket(t.cfg.GCSBucket).Object(objName)
	w := obj.NewWriter(ctx)
	if _, err := w.Write(wav); err != nil {
		return "", fmt.Errorf("gcs upload: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("gcs upload: %w", err)
	}
	defer func() {
		if err := obj.Delete(context.WithoutCancel(ctx)); err != nil {
			slog.Warn("failed to delete gcs object", "object", objName, "err", err)
		}
	}()

	op, err := t.speech.LongRunningRecognize(ctx, &speechpb.LongRunningRecognizeRequest{
		Config: recCfg,
		Audio: &speechpb.RecognitionAudio{
			AudioSource: &speechpb.RecognitionAudio_Uri{
				Uri: fmt.Sprintf("gs://%s/%s", t.cfg.GCSBucket, objName),
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("long running recognize: %w", err)
	}
	opCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	resp, err := op.Wait(opCtx)
	if err != nil {
		return "", fmt.Errorf("long running recognize wait: %w", err)
	}
	return joinResults(resp.Results), nil
}

func joinResults(results []*speechpb.SpeechRecognitionResult) string {
	var b strings.Builder
	for _, r := range results {
		if len(r.Alternatives) > 0 {
			b.WriteString(r.Alternatives[0].Transcript)
		}
	}
	return strings.TrimSpace(b.String())
}

// probeWAV reads duration and sample rate from a WAV header. Returns zero
// values if the data is not parseable as WAV.
func probeWAV(data []byte) (time.Duration, int32) {
	if len(data) < 44 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return 0, 8000
	}
	var sampleRate uint32
	var byteRate uint32
	var dataLen uint32
	pos := 12
	for pos+8 <= len(data) {
		id := string(data[pos : pos+4])
		size := binary.LittleEndian.Uint32(data[pos+4 : pos+8])
		body := pos + 8
		switch id {
		case "fmt ":
			if body+16 <= len(data) {
				sampleRate = binary.LittleEndian.Uint32(data[body+4 : body+8])
				byteRate = binary.LittleEndian.Uint32(data[body+8 : body+12])
			}
		case "data":
			dataLen = size
		}
		pos = body + int(size)
		if size%2 == 1 {
			pos++ // chunks are word-aligned
		}
	}
	if sampleRate == 0 {
		return 0, 8000
	}
	var dur time.Duration
	if byteRate > 0 && dataLen > 0 {
		dur = time.Duration(float64(dataLen) / float64(byteRate) * float64(time.Second))
	}
	return dur, int32(sampleRate)
}
