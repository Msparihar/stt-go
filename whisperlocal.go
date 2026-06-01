//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const defaultLocalWhisperURL = "http://127.0.0.1:5111/transcribe"

// localWhisperBase derives the sidecar's base URL from the configured transcribe
// endpoint so /load and /unload hit the same host:port.
func localWhisperBase() string {
	url := appConfig.LocalWhisperURL
	if url == "" {
		url = defaultLocalWhisperURL
	}
	return strings.TrimSuffix(url, "/transcribe")
}

// loadLocalWhisper asks the sidecar to load the model into VRAM. Fire-and-forget
// from switchBackend so the tray click returns instantly; the model warms in the
// background and is ready by the time the user finishes their first utterance.
func loadLocalWhisper(log *slog.Logger) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "POST", localWhisperBase()+"/load", nil)
		if err != nil {
			return
		}
		resp, err := whisperLocalHTTPClient.Do(req)
		if err != nil {
			log.Warn("[LOCAL] preload failed (sidecar down?)", "err", err)
			return
		}
		resp.Body.Close()
		log.Info("[LOCAL] model preload requested")
	}()
}

// unloadLocalWhisper asks the sidecar to free VRAM when the user switches away
// from the local backend. Fire-and-forget; failure is harmless.
func unloadLocalWhisper(log *slog.Logger) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "POST", localWhisperBase()+"/unload", nil)
		if err != nil {
			return
		}
		resp, err := whisperLocalHTTPClient.Do(req)
		if err != nil {
			return
		}
		resp.Body.Close()
		log.Info("[LOCAL] model unload requested (VRAM freed)")
	}()
}

// localWhisperHealth probes the sidecar's /health endpoint. reachable is true
// when the server answers at all; loaded reports whether the model is in VRAM.
// Used by the tray to show a live connection indicator.
func localWhisperHealth(ctx context.Context) (reachable, loaded bool) {
	req, err := http.NewRequestWithContext(ctx, "GET", localWhisperBase()+"/health", nil)
	if err != nil {
		return false, false
	}
	resp, err := whisperLocalHTTPClient.Do(req)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return true, false
	}
	rb, _ := io.ReadAll(resp.Body)
	var h struct {
		Loaded bool `json:"loaded"`
	}
	_ = json.Unmarshal(rb, &h)
	return true, h.Loaded
}

// transcribeWhisperLocal sends WAV audio to the local faster-whisper sidecar
// (server.py) running on the GPU. Same request shape as the other REST
// backends, but the audio never leaves the machine.
func transcribeWhisperLocal(ctx context.Context, pcm []byte, log *slog.Logger) (string, error) {
	t0 := time.Now()
	duration := float64(len(pcm)) / float64(avgBytesPerSec)
	url := appConfig.LocalWhisperURL
	if url == "" {
		url = defaultLocalWhisperURL
	}
	log.Info("[LOCAL] request start", "url", url, "duration", fmt.Sprintf("%.1fs", duration))

	wav := pcmToWAV(pcm)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(wav))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "audio/wav")
	req.Header.Set("X-Language", appConfig.Language)
	req.Header.Set("X-Prompt", whisperPrompt) // keyterm biasing, parity with OpenAI backend

	resp, err := whisperLocalHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP: %w (is the sidecar running? D:\\whisper-local\\server.py)", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", &httpStatusError{StatusCode: resp.StatusCode, Body: string(rb)}
	}

	var res struct{ Text string }
	if err := json.Unmarshal(rb, &res); err != nil {
		log.Error("[LOCAL] unmarshal failed", "err", err, "body", fmt.Sprintf("%.200q", string(rb)))
		return "", fmt.Errorf("unmarshal response: %w", err)
	}

	text := strings.TrimSpace(res.Text)
	snippet := text
	if len(snippet) > 80 {
		snippet = snippet[:80] + "..."
	}
	log.Info("[LOCAL] response received", "elapsed", time.Since(t0).Round(time.Millisecond), "text_len", len(text), "text", snippet)
	return text, nil
}
