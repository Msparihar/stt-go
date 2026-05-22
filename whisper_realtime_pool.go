//go:build windows

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	rtIdleTimeout      = 2 * time.Minute
	rtKeepaliveInterval = 20 * time.Second
	rtReadDeadline     = 20 * time.Second
)

// realtimeClient holds a persistent WebSocket connection to the OpenAI Realtime
// transcription API. Only one transcription runs at a time (mu serialises access).
type realtimeClient struct {
	mu             sync.Mutex
	conn           *websocket.Conn
	lastUsed       time.Time
	idleTimer      *time.Timer
	keepaliveStop  chan struct{}
	apiKey         string
	log            *slog.Logger
}

// package-level singleton, guarded by realtimePoolMu for creation.
var (
	realtimePool    *realtimeClient
	realtimePoolMu  sync.Mutex
)

// getRealtimePool returns the singleton, creating it on first call.
func getRealtimePool(apiKey string, log *slog.Logger) *realtimeClient {
	realtimePoolMu.Lock()
	defer realtimePoolMu.Unlock()
	if realtimePool == nil {
		realtimePool = &realtimeClient{apiKey: apiKey, log: log}
	}
	// Update apiKey in case it changed (backend switch).
	realtimePool.apiKey = apiKey
	realtimePool.log = log
	return realtimePool
}

// dial opens a new WebSocket connection and sends the session config.
// Caller must hold r.mu.
func (r *realtimeClient) dial(ctx context.Context, lang string) error {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	headers := http.Header{
		"Authorization": {"Bearer " + r.apiKey},
		"OpenAI-Beta":   {"realtime=v1"},
	}

	r.log.Info("[WH-RT-POOL] opening connection", "url", realtimeURL)
	conn, resp, err := dialer.DialContext(ctx, realtimeURL, headers)
	if err != nil {
		statusCode := 0
		if resp != nil {
			statusCode = resp.StatusCode
		}
		r.log.Error("[WH-RT-POOL] dial failed", "err", err, "status", statusCode)
		if statusCode == 401 {
			return fmt.Errorf("[WH-RT-POOL] auth failed (401): %w", err)
		}
		return fmt.Errorf("WebSocket dial: %w", err)
	}

	// Pong handler resets the read deadline so keepalive pings extend the
	// idle read window while no transcription is in progress.
	conn.SetPongHandler(func(string) error {
		r.log.Info("[WH-RT-POOL] pong received")
		// Only clear if we're between transcriptions (no active read deadline).
		conn.SetReadDeadline(time.Time{})
		return nil
	})

	r.conn = conn
	r.log.Info("[WH-RT-POOL] connection opened")

	// Send session config.
	if err := r.sendSessionConfig(lang); err != nil {
		r.conn.Close()
		r.conn = nil
		return fmt.Errorf("session config: %w", err)
	}

	// Start keepalive goroutine.
	stopCh := make(chan struct{})
	r.keepaliveStop = stopCh
	go r.runKeepalive(stopCh)

	// Arm idle timer.
	r.idleTimer = time.AfterFunc(rtIdleTimeout, r.closeIdle)
	r.lastUsed = time.Now()

	return nil
}

// sendSessionConfig sends transcription_session.update on an existing conn.
// Caller must hold r.mu.
func (r *realtimeClient) sendSessionConfig(lang string) error {
	prompt := whisperPrompt
	if len(prompt) > 1024 {
		cut := strings.LastIndex(prompt[:1024], " ")
		if cut < 512 {
			cut = 1024
		}
		prompt = prompt[:cut]
		r.log.Info("[WH-RT-POOL] prompt truncated", "orig", len(whisperPrompt), "new", len(prompt))
	}

	sessionUpdate := map[string]interface{}{
		"type": "transcription_session.update",
		"session": map[string]interface{}{
			"input_audio_format": "pcm16",
			"input_audio_transcription": map[string]interface{}{
				"model":    "gpt-4o-mini-transcribe",
				"language": lang,
				"prompt":   prompt,
			},
			"turn_detection": nil,
		},
	}
	if err := writeJSON(r.conn, sessionUpdate); err != nil {
		return err
	}
	r.log.Info("[WH-RT-POOL] session config sent")
	return nil
}

// runKeepalive sends a WebSocket ping every rtKeepaliveInterval.
// It exits when stopCh is closed.
func (r *realtimeClient) runKeepalive(stopCh chan struct{}) {
	ticker := time.NewTicker(rtKeepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			r.mu.Lock()
			if r.conn == nil {
				r.mu.Unlock()
				return
			}
			err := r.conn.WriteMessage(websocket.PingMessage, nil)
			r.mu.Unlock()
			if err != nil {
				r.log.Warn("[WH-RT-POOL] keepalive ping failed", "err", err)
				return
			}
			r.log.Info("[WH-RT-POOL] keepalive ping sent")
		}
	}
}

// closeIdle is called by the idle timer; closes the connection without holding
// a caller lock (uses its own mu lock).
func (r *realtimeClient) closeIdle() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn == nil {
		return
	}
	r.log.Info("[WH-RT-POOL] idle timeout — closing connection")
	r.closeConnLocked()
}

// Close tears down the connection immediately (e.g. on app shutdown).
func (r *realtimeClient) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn == nil {
		return
	}
	r.log.Info("[WH-RT-POOL] explicit close")
	r.closeConnLocked()
}

// closeConnLocked closes the WebSocket and cleans up timers/goroutines.
// Caller must hold r.mu.
func (r *realtimeClient) closeConnLocked() {
	if r.keepaliveStop != nil {
		close(r.keepaliveStop)
		r.keepaliveStop = nil
	}
	if r.idleTimer != nil {
		r.idleTimer.Stop()
		r.idleTimer = nil
	}
	r.conn.Close()
	r.conn = nil
}

// Transcribe is the main entry point. It acquires the mutex (serialising calls),
// ensures a live connection, streams pcm, and returns the transcript.
func (r *realtimeClient) Transcribe(ctx context.Context, pcm []byte, lang string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t0 := time.Now()
	duration := float64(len(pcm)) / float64(avgBytesPerSec)
	r.log.Info("[WH-RT] preparing audio", "pcmBytes", len(pcm), "duration", fmt.Sprintf("%.1fs", duration))

	// Ensure we have a live connection.
	if r.conn != nil {
		// Quick liveness check: attempt a zero-byte control-frame write.
		// gorilla/websocket will return an error if the underlying TCP is gone.
		if err := r.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
			r.log.Warn("[WH-RT-POOL] existing conn is dead, re-dialing", "err", err)
			r.closeConnLocked()
		} else {
			r.log.Info("[WH-RT-POOL] reusing existing connection")
		}
	}

	if r.conn == nil {
		if err := r.dial(ctx, lang); err != nil {
			return "", err
		}
		// Consume the session.created / session.updated events that arrive
		// immediately after dialing, before we send audio.
		if err := r.drainSetupEvents(ctx); err != nil {
			r.log.Warn("[WH-RT-POOL] error draining setup events (continuing)", "err", err)
		}
	} else {
		// Re-send session config for the new utterance (language may have changed
		// and it resets any prior audio state on the server).
		if err := r.sendSessionConfig(lang); err != nil {
			r.log.Warn("[WH-RT-POOL] session update failed, re-dialing", "err", err)
			r.closeConnLocked()
			if err2 := r.dial(ctx, lang); err2 != nil {
				return "", err2
			}
			if err2 := r.drainSetupEvents(ctx); err2 != nil {
				r.log.Warn("[WH-RT-POOL] error draining setup events after re-dial", "err", err2)
			}
		} else {
			if err := r.drainSetupEvents(ctx); err != nil {
				r.log.Warn("[WH-RT-POOL] error draining session.updated events (continuing)", "err", err)
			}
		}
	}

	// Reset idle timer.
	r.lastUsed = time.Now()
	if r.idleTimer != nil {
		r.idleTimer.Reset(rtIdleTimeout)
	}

	// ── Stream audio in chunks ──────────────────────────────────────
	chunkSize := avgBytesPerSec * bufDurationMs / 1000
	sentChunks := 0
	for offset := 0; offset < len(pcm); offset += chunkSize {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		end := offset + chunkSize
		if end > len(pcm) {
			end = len(pcm)
		}
		encoded := base64.StdEncoding.EncodeToString(pcm[offset:end])
		appendMsg := map[string]interface{}{
			"type":  "input_audio_buffer.append",
			"audio": encoded,
		}
		if err := writeJSON(r.conn, appendMsg); err != nil {
			r.log.Error("[WH-RT] audio chunk write failed", "chunk", sentChunks, "err", err)
			r.closeConnLocked()
			return "", fmt.Errorf("send audio chunk %d: %w", sentChunks, err)
		}
		sentChunks++
	}
	r.log.Info("[WH-RT] audio streamed", "chunks", sentChunks, "bytes", len(pcm))

	// ── Commit audio buffer ─────────────────────────────────────────
	commitMsg := map[string]interface{}{"type": "input_audio_buffer.commit"}
	if err := writeJSON(r.conn, commitMsg); err != nil {
		r.log.Error("[WH-RT] commit write failed", "err", err)
		r.closeConnLocked()
		return "", fmt.Errorf("send commit: %w", err)
	}
	r.log.Info("[WH-RT] audio buffer committed, waiting for transcription")

	// ── Read events until transcript complete ───────────────────────
	// Set a per-call read deadline; clear it when done so keepalive pongs
	// don't hit a stale deadline.
	r.conn.SetReadDeadline(time.Now().Add(rtReadDeadline))
	readDeadline := time.Now().Add(rtReadDeadline)

	var deltaBuffer strings.Builder
	var finalText string

	text, err := func() (string, error) {
		for {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}

			_, msgBytes, err := r.conn.ReadMessage()
			if err != nil {
				if time.Now().After(readDeadline) {
					r.log.Warn("[WH-RT] read deadline exceeded, returning partial")
					return strings.TrimSpace(deltaBuffer.String()), nil
				}
				return "", fmt.Errorf("read: %w", err)
			}

			var event struct {
				Type       string `json:"type"`
				EventID    string `json:"event_id"`
				Transcript string `json:"transcript"`
				Delta      string `json:"delta"`
				Item       *struct {
					Content []struct {
						Type       string `json:"type"`
						Transcript string `json:"transcript"`
					} `json:"content"`
				} `json:"item"`
			}
			if err := json.Unmarshal(msgBytes, &event); err != nil {
				r.log.Warn("[WH-RT] failed to parse event", "err", err)
				continue
			}

			r.log.Info("[WH-RT] event received", "type", event.Type)

			switch event.Type {
			case "conversation.item.input_audio_transcription.delta":
				deltaBuffer.WriteString(event.Delta)
				r.log.Info("[WH-RT] transcript delta", "delta", event.Delta)

			case "conversation.item.input_audio_transcription.completed":
				if event.Transcript != "" {
					t := strings.TrimSpace(event.Transcript)
					r.log.Info("[WH-RT] transcript completed", "text", t)
					return t, nil
				}
				if event.Item != nil {
					for _, c := range event.Item.Content {
						if c.Transcript != "" {
							t := strings.TrimSpace(c.Transcript)
							r.log.Info("[WH-RT] transcript from item content", "text", t)
							return t, nil
						}
					}
				}
				t := strings.TrimSpace(deltaBuffer.String())
				r.log.Info("[WH-RT] transcript from deltas (no direct text)", "text", t)
				return t, nil

			case "error":
				r.log.Error("[WH-RT] server error event", "raw", string(msgBytes))
				return "", fmt.Errorf("server error: %s", string(msgBytes))

			case "session.created", "session.updated",
				"input_audio_buffer.committed", "input_audio_buffer.cleared",
				"conversation.item.created", "response.created",
				"response.done", "response.output_item.added",
				"response.output_item.done":
				// Informational — no action needed
			}
		}
	}()

	// Clear the per-call read deadline so idle pongs don't time out.
	if r.conn != nil {
		r.conn.SetReadDeadline(time.Time{})
	}

	if err != nil {
		r.log.Error("[WH-RT] transcription failed", "err", err)
		r.closeConnLocked()
		return "", err
	}

	if finalText == "" {
		finalText = text
	}

	// Update idle timer after successful use.
	if r.idleTimer != nil {
		r.idleTimer.Reset(rtIdleTimeout)
	}

	r.log.Info("[WH-RT] completed",
		"elapsed", time.Since(t0).Round(time.Millisecond),
		"text", finalText,
	)
	return finalText, nil
}

// writeJSON serialises v and sends it as a text WebSocket message.
func writeJSON(conn *websocket.Conn, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, data)
}

// closeRealtimePool closes the singleton pool connection if it exists.
// Safe to call even if the pool was never used.
func closeRealtimePool() {
	realtimePoolMu.Lock()
	p := realtimePool
	realtimePoolMu.Unlock()
	if p != nil {
		p.Close()
	}
}

// drainSetupEvents reads and discards setup-phase events (session.created,
// session.updated) that arrive right after a dial or session update. It stops
// when it sees session.updated or times out after 3 seconds.
func (r *realtimeClient) drainSetupEvents(ctx context.Context) error {
	r.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	defer r.conn.SetReadDeadline(time.Time{})

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, msgBytes, err := r.conn.ReadMessage()
		if err != nil {
			// Timeout is expected here — just means no more setup events.
			return nil
		}
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(msgBytes, &event) != nil {
			continue
		}
		r.log.Info("[WH-RT-POOL] setup event", "type", event.Type)
		if event.Type == "session.updated" || event.Type == "error" {
			return nil
		}
	}
}
