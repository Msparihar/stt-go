//go:build windows

package main

import (
	"encoding/binary"
	"testing"
)

func TestPcmToWAV_HeaderLayout(t *testing.T) {
	wav := pcmToWAV([]byte{})
	if len(wav) < 44 {
		t.Fatalf("WAV header too short: %d bytes", len(wav))
	}

	if string(wav[0:4]) != "RIFF" {
		t.Errorf("bytes 0-3: want RIFF, got %q", wav[0:4])
	}
	if string(wav[8:12]) != "WAVE" {
		t.Errorf("bytes 8-11: want WAVE, got %q", wav[8:12])
	}
	if string(wav[12:16]) != "fmt " {
		t.Errorf("bytes 12-15: want 'fmt ', got %q", wav[12:16])
	}
	if string(wav[36:40]) != "data" {
		t.Errorf("bytes 36-39: want 'data', got %q", wav[36:40])
	}

	sr := binary.LittleEndian.Uint32(wav[24:28])
	if sr != 16000 {
		t.Errorf("sample rate at offset 24: want 16000, got %d", sr)
	}
}
