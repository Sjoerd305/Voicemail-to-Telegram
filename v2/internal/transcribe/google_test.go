package transcribe

import (
	"encoding/binary"
	"testing"
	"time"
)

func makeWAV(sampleRate uint32, seconds int) []byte {
	byteRate := sampleRate * 2 // 16-bit mono
	dataLen := byteRate * uint32(seconds)
	buf := make([]byte, 44) // header only; data chunk size lies about body, fine for probing
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], 36+dataLen)
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(buf[22:24], 1) // mono
	binary.LittleEndian.PutUint32(buf[24:28], sampleRate)
	binary.LittleEndian.PutUint32(buf[28:32], byteRate)
	binary.LittleEndian.PutUint16(buf[32:34], 2)
	binary.LittleEndian.PutUint16(buf[34:36], 16)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], dataLen)
	return buf
}

func TestProbeWAV(t *testing.T) {
	dur, rate := probeWAV(makeWAV(8000, 42))
	if rate != 8000 {
		t.Fatalf("sample rate: got %d", rate)
	}
	if dur.Round(time.Second) != 42*time.Second {
		t.Fatalf("duration: got %s", dur)
	}
}

func TestProbeWAVGarbage(t *testing.T) {
	dur, rate := probeWAV([]byte("not a wav file at all, definitely not"))
	if dur != 0 || rate != 8000 {
		t.Fatalf("expected fallback values, got %s / %d", dur, rate)
	}
}
