//go:build windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// ipv4Dialer forces connections over IPv4 to avoid IPv6 timeout delays.
var ipv4Dialer = &websocket.Dialer{
	NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp4", addr)
	},
	HandshakeTimeout: 10 * time.Second,
}

// ── Deepgram result types ───────────────────────────────────────────

type dgResult struct {
	Type            string `json:"type"`
	IsFinal         bool   `json:"is_final"`
	SpeechFinalized bool   `json:"speech_finalized"`
	Channel         struct {
		Alternatives []struct {
			Transcript string `json:"transcript"`
		} `json:"alternatives"`
	} `json:"channel"`
}

// ── On-demand Deepgram connection ─────────────────────────────────
//
// WebSocket connects when recording starts (on hotkey press).
// Audio is buffered until the connection is ready, then flushed.
// Connection is closed after each transcription completes.

type dgConn struct {
	apiKey string
	lang   string
	log    *slog.Logger
	wsURL  string

	mu      sync.Mutex
	conn    *websocket.Conn
	ready   bool
	readyCh chan struct{} // closed when connect succeeds for this session
	session atomic.Int64  // incremented on each startRecording; stale goroutines check this

	// audio buffer — holds chunks until WebSocket is ready
	bufMu  sync.Mutex
	buffer [][]byte

	// per-recording state
	recMu      sync.Mutex
	recParts   []string
	recDone    chan struct{}
	recActive  bool
	finalizing bool
	dropped    bool      // true if connection dropped during active recording
	flushTime  time.Time // when buffered audio was flushed to Deepgram
}

func newDGConn(apiKey, lang string, log *slog.Logger) *dgConn {
	params := url.Values{
		"model":            {"nova-3"},
		"language":         {lang},
		"encoding":         {"linear16"},
		"sample_rate":      {fmt.Sprintf("%d", sampleRate)},
		"channels":         {fmt.Sprintf("%d", audioCh)},
		"punctuate":        {"true"},
		"smart_format":     {"true"},
		"interim_results":  {"true"},
		"vad_events":       {"true"},
		"endpointing":      {"300"},
		"utterance_end_ms": {"1000"},
	}
	for _, kt := range techTerms {
		params.Add("keyterm", kt)
	}

	closed := make(chan struct{})
	close(closed)
	return &dgConn{
		apiKey:  apiKey,
		lang:    lang,
		log:     log,
		wsURL:   "wss://api.deepgram.com/v1/listen?" + params.Encode(),
		readyCh: closed, // starts as closed (no active session)
		recDone: closed,
	}
}

func (dc *dgConn) close() {
	dc.mu.Lock()
	if dc.conn != nil {
		dc.conn.Close()
		dc.conn = nil
	}
	dc.ready = false
	dc.mu.Unlock()
	dc.log.Info("Deepgram connection closed")
}

// connect dials Deepgram and flushes buffered audio once connected.
// Called once per recording session. mySession identifies which session
// spawned this goroutine — if the session has moved on, we bail out.
func (dc *dgConn) connect(mySession int64) {
	dc.log.Info("Deepgram connecting...")
	t0 := time.Now()

	conn, _, err := ipv4Dialer.Dial(dc.wsURL, http.Header{
		"Authorization": {"Token " + dc.apiKey},
	})
	if err != nil {
		dc.log.Error("Deepgram connect failed", "err", err, "elapsed", time.Since(t0).Round(time.Millisecond))
		return
	}

	// Check if this session is still current before touching shared state
	if dc.session.Load() != mySession {
		dc.log.Warn("Deepgram stale connect goroutine, closing connection", "mySession", mySession, "currentSession", dc.session.Load())
		conn.Close()
		return
	}

	dc.mu.Lock()
	dc.conn = conn
	dc.ready = true
	dc.mu.Unlock()
	dc.log.Info("Deepgram connected", "elapsed", time.Since(t0).Round(time.Millisecond))

	// Signal that connection is ready
	close(dc.readyCh)

	// Flush buffered audio
	dc.bufMu.Lock()
	buffered := dc.buffer
	dc.buffer = nil
	dc.bufMu.Unlock()

	if len(buffered) > 0 {
		dc.mu.Lock()
		for _, chunk := range buffered {
			conn.WriteMessage(websocket.BinaryMessage, chunk)
		}
		dc.mu.Unlock()
		dc.recMu.Lock()
		dc.flushTime = time.Now()
		dc.recMu.Unlock()
		dc.log.Info("Deepgram flushed buffered audio", "chunks", len(buffered))
	}

	// Read loop runs until connection drops or session ends
	dc.readLoop(conn)

	// Connection ended — only clean up if we're still the current session
	if dc.session.Load() != mySession {
		return
	}

	dc.mu.Lock()
	dc.ready = false
	dc.conn = nil
	dc.mu.Unlock()

	// If recording was still active, mark as dropped and unblock finalize
	dc.recMu.Lock()
	if dc.recActive {
		dc.dropped = true
		close(dc.recDone)
		dc.recActive = false
		dc.finalizing = false
	}
	dc.recMu.Unlock()
}

func (dc *dgConn) readLoop(conn *websocket.Conn) {
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			// Connection closed (expected after CloseStream) — signal recDone
			dc.recMu.Lock()
			if dc.recActive && dc.finalizing {
				dc.log.Info("Deepgram readLoop: connection closed after CloseStream, signaling done",
					"parts_collected", len(dc.recParts))
				close(dc.recDone)
				dc.recActive = false
				dc.finalizing = false
			}
			dc.recMu.Unlock()
			return
		}
		var r dgResult
		if json.Unmarshal(msg, &r) != nil {
			continue
		}

		// Log every message for debugging truncation issues
		transcript := ""
		if len(r.Channel.Alternatives) > 0 {
			transcript = r.Channel.Alternatives[0].Transcript
		}
		dc.log.Info("Deepgram msg",
			"type", r.Type,
			"is_final", r.IsFinal,
			"speech_finalized", r.SpeechFinalized,
			"text", transcript,
		)

		if r.Type == "Results" && r.IsFinal {
			dc.recMu.Lock()
			t := strings.TrimSpace(transcript)
			if t != "" {
				dc.recParts = append(dc.recParts, t)
			}
			// Don't close recDone here — wait for the connection to close after
			// CloseStream. Deepgram may send multiple is_final results (one per
			// utterance segment) before closing. Closing recDone on the first one
			// causes trailing segments to be lost.
			dc.recMu.Unlock()
		}
	}
}

// startRecording resets state and kicks off a new connection.
func (dc *dgConn) startRecording() {
	// Bump session so any in-flight connect goroutine knows it's stale
	sess := dc.session.Add(1)

	// Reset per-recording state
	dc.recMu.Lock()
	dc.recParts = nil
	dc.recDone = make(chan struct{})
	dc.recActive = true
	dc.finalizing = false
	dc.dropped = false
	dc.flushTime = time.Time{}
	dc.recMu.Unlock()

	// Reset buffer and connection state
	dc.bufMu.Lock()
	dc.buffer = nil
	dc.bufMu.Unlock()

	dc.mu.Lock()
	if dc.conn != nil {
		dc.conn.Close()
		dc.conn = nil
	}
	dc.ready = false
	dc.readyCh = make(chan struct{})
	dc.mu.Unlock()

	// Connect in background — audio will buffer until ready
	go dc.connect(sess)
}

// send streams a PCM chunk to Deepgram, or buffers it if not yet connected.
func (dc *dgConn) send(data []byte) {
	dc.mu.Lock()
	conn := dc.conn
	ready := dc.ready
	dc.mu.Unlock()

	if !ready || conn == nil {
		// Buffer until connected
		chunk := make([]byte, len(data))
		copy(chunk, data)
		dc.bufMu.Lock()
		dc.buffer = append(dc.buffer, chunk)
		dc.bufMu.Unlock()
		return
	}

	dc.mu.Lock()
	conn.WriteMessage(websocket.BinaryMessage, data)
	dc.mu.Unlock()
}

// finalize signals end of utterance, waits for final transcript, then closes connection.
func (dc *dgConn) finalize(t0 time.Time) string {
	finishStart := time.Now()

	// Wait for connection if still connecting (with timeout)
	select {
	case <-dc.readyCh:
	case <-time.After(5 * time.Second):
		dc.log.Warn("Deepgram connect timeout during finalize")
		dc.recMu.Lock()
		dc.dropped = true
		dc.recMu.Unlock()
		dc.close()
		return ""
	}

	dc.mu.Lock()
	conn := dc.conn
	ready := dc.ready
	dc.mu.Unlock()

	dc.recMu.Lock()
	doneCh := dc.recDone
	dc.recMu.Unlock()

	if !ready || conn == nil {
		dc.log.Warn("Deepgram not connected at finalize")
		dc.recMu.Lock()
		dc.dropped = true
		dc.recMu.Unlock()
		return ""
	}

	dc.recMu.Lock()
	dc.finalizing = true
	dc.recMu.Unlock()

	// Send CloseStream instead of Finalize — tells Deepgram "no more audio,
	// finish processing everything in your pipeline". Finalize was causing
	// truncation because it returns partial results before the pipeline flushes.
	dc.mu.Lock()
	conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"CloseStream"}`))
	dc.mu.Unlock()

	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		dc.recMu.Lock()
		dc.recActive = false
		dc.finalizing = false
		dc.recMu.Unlock()
	}

	dc.recMu.Lock()
	text := strings.Join(dc.recParts, " ")
	dc.recMu.Unlock()

	postRelease := time.Since(finishStart)
	session := time.Since(t0)
	if text != "" {
		dc.log.Info("Deepgram transcription", "post_release", postRelease.Round(time.Millisecond), "session", session.Round(time.Millisecond), "text", text)
	} else {
		dc.log.Info("Deepgram: no speech detected", "session", session.Round(time.Millisecond))
	}

	// Close connection — next recording will open a fresh one
	dc.close()

	return text
}

func (dc *dgConn) wasDropped() bool {
	dc.recMu.Lock()
	defer dc.recMu.Unlock()
	return dc.dropped
}
