//go:build windows

package main

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// ── ElevenLabs WebSocket message types ──────────────────────────────

type elSendMsg struct {
	MessageType  string `json:"message_type"`
	AudioBase64  string `json:"audio_base_64"`
	Commit       bool   `json:"commit"`
	SampleRate   int    `json:"sample_rate"`
	PreviousText string `json:"previous_text,omitempty"` // context hint, first chunk only
}

type elRecvMsg struct {
	MessageType string `json:"message_type"`
	Text        string `json:"text,omitempty"`
	Error       string `json:"error,omitempty"`
}

// ── On-demand ElevenLabs connection ─────────────────────────────────
//
// Same pattern as Deepgram: connect on hotkey press, buffer audio
// until WebSocket is ready, flush, stream, commit on release.

type elConn struct {
	apiKey string
	lang   string
	log    *slog.Logger
	wsURL  string

	mu      sync.Mutex
	conn    *websocket.Conn
	ready   bool
	readyCh chan struct{}
	session atomic.Int64

	// audio buffer
	bufMu  sync.Mutex
	buffer [][]byte

	// per-recording state
	recMu      sync.Mutex
	recParts   []string
	recDone    chan struct{}
	recActive  bool
	committed  bool // true after commit sent
	firstChunk bool // true until first audio chunk is sent
}

func newELConn(apiKey, lang string, log *slog.Logger) *elConn {
	closed := make(chan struct{})
	close(closed)
	return &elConn{
		apiKey:  apiKey,
		lang:    lang,
		log:     log,
		wsURL:   "wss://api.elevenlabs.io/v1/speech-to-text/realtime?model_id=scribe_v2_realtime&language_code=" + lang,
		readyCh: closed,
		recDone: closed,
	}
}

func (ec *elConn) close() {
	ec.mu.Lock()
	if ec.conn != nil {
		ec.conn.Close()
		ec.conn = nil
	}
	ec.ready = false
	ec.mu.Unlock()
	ec.log.Info("ElevenLabs connection closed")
}

func (ec *elConn) connect(mySession int64) {
	ec.log.Info("ElevenLabs connecting...")
	t0 := time.Now()

	conn, _, err := ipv4Dialer.Dial(ec.wsURL, http.Header{
		"xi-api-key": {ec.apiKey},
	})
	if err != nil {
		ec.log.Error("ElevenLabs connect failed", "err", err, "elapsed", time.Since(t0).Round(time.Millisecond))
		return
	}

	// Wait for session_started message
	_, msg, err := conn.ReadMessage()
	if err != nil {
		ec.log.Error("ElevenLabs failed to read session_started", "err", err)
		conn.Close()
		return
	}
	var initMsg elRecvMsg
	json.Unmarshal(msg, &initMsg)
	if initMsg.MessageType != "session_started" {
		ec.log.Warn("ElevenLabs unexpected first message", "type", initMsg.MessageType)
	}

	// Check if still current session
	if ec.session.Load() != mySession {
		ec.log.Warn("ElevenLabs stale connect goroutine, closing", "mySession", mySession)
		conn.Close()
		return
	}

	ec.mu.Lock()
	ec.conn = conn
	ec.ready = true
	ec.mu.Unlock()
	ec.log.Info("ElevenLabs connected", "elapsed", time.Since(t0).Round(time.Millisecond))

	close(ec.readyCh)

	// Flush buffered audio
	ec.bufMu.Lock()
	buffered := ec.buffer
	ec.buffer = nil
	ec.bufMu.Unlock()

	if len(buffered) > 0 {
		ec.mu.Lock()
		for i, chunk := range buffered {
			if i == 0 {
				ec.writeAudioChunk(conn, chunk, false, whisperPrompt)
				ec.recMu.Lock()
				ec.firstChunk = false
				ec.recMu.Unlock()
			} else {
				ec.writeAudioChunk(conn, chunk, false, "")
			}
		}
		ec.mu.Unlock()
		ec.log.Info("ElevenLabs flushed buffered audio", "chunks", len(buffered))
	}

	// Read loop
	ec.readLoop(conn)

	if ec.session.Load() != mySession {
		return
	}

	ec.mu.Lock()
	ec.ready = false
	ec.conn = nil
	ec.mu.Unlock()

	ec.recMu.Lock()
	if ec.recActive {
		close(ec.recDone)
		ec.recActive = false
		ec.committed = false
	}
	ec.recMu.Unlock()
}

func (ec *elConn) writeAudioChunk(conn *websocket.Conn, pcmData []byte, commit bool, previousText string) {
	// previous_text is capped at 50 chars by the ElevenLabs realtime API
	if len(previousText) > 50 {
		previousText = previousText[:50]
	}
	msg := elSendMsg{
		MessageType:  "input_audio_chunk",
		AudioBase64:  base64.StdEncoding.EncodeToString(pcmData),
		Commit:       commit,
		SampleRate:   sampleRate,
		PreviousText: previousText,
	}
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)
}

func (ec *elConn) readLoop(conn *websocket.Conn) {
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var r elRecvMsg
		if json.Unmarshal(msg, &r) != nil {
			continue
		}

		switch r.MessageType {
		case "partial_transcript":
			// Interim result — ignore for now (we only need final)
		case "committed_transcript":
			ec.recMu.Lock()
			text := strings.TrimSpace(r.Text)
			if text != "" {
				ec.recParts = append(ec.recParts, text)
			}
			if ec.recActive && ec.committed {
				close(ec.recDone)
				ec.recActive = false
				ec.committed = false
			}
			ec.recMu.Unlock()
		case "input_error":
			ec.log.Error("ElevenLabs input error", "error", r.Error)
		}
	}
}

func (ec *elConn) startRecording() {
	sess := ec.session.Add(1)

	ec.recMu.Lock()
	ec.recParts = nil
	ec.recDone = make(chan struct{})
	ec.recActive = true
	ec.committed = false
	ec.firstChunk = true
	ec.recMu.Unlock()

	ec.bufMu.Lock()
	ec.buffer = nil
	ec.bufMu.Unlock()

	ec.mu.Lock()
	if ec.conn != nil {
		ec.conn.Close()
		ec.conn = nil
	}
	ec.ready = false
	ec.readyCh = make(chan struct{})
	ec.mu.Unlock()

	go ec.connect(sess)
}

func (ec *elConn) send(data []byte) {
	ec.mu.Lock()
	conn := ec.conn
	ready := ec.ready
	ec.mu.Unlock()

	if !ready || conn == nil {
		chunk := make([]byte, len(data))
		copy(chunk, data)
		ec.bufMu.Lock()
		ec.buffer = append(ec.buffer, chunk)
		ec.bufMu.Unlock()
		return
	}

	ec.recMu.Lock()
	isFirst := ec.firstChunk
	if isFirst {
		ec.firstChunk = false
	}
	ec.recMu.Unlock()

	prevText := ""
	if isFirst {
		prevText = whisperPrompt
	}

	ec.mu.Lock()
	ec.writeAudioChunk(conn, data, false, prevText)
	ec.mu.Unlock()
}

func (ec *elConn) finalize(t0 time.Time) string {
	finishStart := time.Now()

	select {
	case <-ec.readyCh:
	case <-time.After(5 * time.Second):
		ec.log.Warn("ElevenLabs connect timeout during finalize")
		ec.close()
		return ""
	}

	ec.mu.Lock()
	conn := ec.conn
	ready := ec.ready
	ec.mu.Unlock()

	ec.recMu.Lock()
	doneCh := ec.recDone
	ec.recMu.Unlock()

	if !ready || conn == nil {
		ec.log.Warn("ElevenLabs not connected at finalize")
		return ""
	}

	// Send commit via input_audio_chunk with commit=true (raw WebSocket protocol;
	// the {"message_type":"commit"} form is an SDK abstraction, not the wire format)
	ec.recMu.Lock()
	ec.committed = true
	ec.recMu.Unlock()

	ec.mu.Lock()
	ec.writeAudioChunk(conn, nil, true, "")
	ec.mu.Unlock()

	select {
	case <-doneCh:
	case <-time.After(3 * time.Second):
		ec.recMu.Lock()
		ec.recActive = false
		ec.committed = false
		ec.recMu.Unlock()
	}

	ec.recMu.Lock()
	text := strings.Join(ec.recParts, " ")
	ec.recMu.Unlock()

	postRelease := time.Since(finishStart)
	session := time.Since(t0)
	if text != "" {
		ec.log.Info("ElevenLabs transcription", "post_release", postRelease.Round(time.Millisecond), "session", session.Round(time.Millisecond), "text", text)
	} else {
		ec.log.Info("ElevenLabs: no speech detected", "session", session.Round(time.Millisecond))
	}

	ec.close()
	return text
}
