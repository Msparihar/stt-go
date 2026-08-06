package main

import (
	"context"
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
	dropped    bool // true if WS connection died mid-recording (before clean commit)
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
	ec.log.Info("[EL] connection closed")
}

func (ec *elConn) connect(mySession int64) {
	ec.log.Info("[EL] connecting...")
	t0 := time.Now()

	conn, _, err := ipv4Dialer.Dial(ec.wsURL, http.Header{
		"xi-api-key": {ec.apiKey},
	})
	if err != nil {
		ec.log.Error("[EL] connect failed", "err", err, "elapsed", time.Since(t0).Round(time.Millisecond))
		ec.recMu.Lock()
		if ec.recActive {
			ec.dropped = true
			close(ec.recDone)
			ec.recActive = false
		}
		ec.recMu.Unlock()
		// Unblock anyone waiting on readyCh
		select {
		case <-ec.readyCh:
		default:
			close(ec.readyCh)
		}
		return
	}

	// Wait for session_started message
	_, msg, err := conn.ReadMessage()
	if err != nil {
		ec.log.Error("[EL] failed to read session_started", "err", err)
		conn.Close()
		return
	}
	var initMsg elRecvMsg
	json.Unmarshal(msg, &initMsg)
	if initMsg.MessageType != "session_started" {
		ec.log.Warn("[EL] unexpected first message", "type", initMsg.MessageType)
	}

	// Check if still current session
	if ec.session.Load() != mySession {
		ec.log.Warn("[EL] stale connect goroutine, closing", "mySession", mySession)
		conn.Close()
		return
	}

	ec.mu.Lock()
	ec.conn = conn
	ec.ready = true
	ec.mu.Unlock()
	ec.log.Info("[EL] connected", "elapsed", time.Since(t0).Round(time.Millisecond))

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
		ec.log.Info("[EL] flushed buffered audio", "chunks", len(buffered))
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

	// readLoop returned. If recording is still active, the WS died before
	// a clean commit — flag dropped so the race prefers REST fallbacks.
	ec.recMu.Lock()
	if ec.recActive {
		ec.dropped = true
		ec.log.Warn("[EL] WS dropped mid-recording — flagging for REST fallback",
			"parts_collected", len(ec.recParts))
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
		case "error":
			ec.log.Error("[EL] server error", "code", r.Error)
			// Unblock finalize() so it doesn't sit waiting on recDone
			// for a commit that will never arrive. Flag dropped so the
			// race prefers REST fallbacks over the (likely empty) result.
			ec.recMu.Lock()
			if ec.recActive {
				ec.dropped = true
				close(ec.recDone)
				ec.recActive = false
				ec.committed = false
			}
			ec.recMu.Unlock()
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
	ec.dropped = false
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

// finalize signals end of utterance and waits for the committed transcript.
// Honors ctx: if cancelled (e.g. REST backend already won the race), returns
// whatever has accumulated and closes the connection.
func (ec *elConn) finalize(ctx context.Context, t0 time.Time) string {
	finishStart := time.Now()

	connectCtx, connectCancel := context.WithTimeout(ctx, 5*time.Second)
	defer connectCancel()
	select {
	case <-ec.readyCh:
	case <-connectCtx.Done():
		if ctx.Err() != nil {
			ec.log.Info("[EL] finalize cancelled before connect (race already won)")
		} else {
			ec.log.Warn("[EL] connect timeout during finalize")
			ec.recMu.Lock()
			ec.dropped = true
			ec.recActive = false
			ec.recMu.Unlock()
		}
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
		ec.log.Warn("[EL] not connected at finalize")
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

	waitCtx, waitCancel := context.WithTimeout(ctx, 3*time.Second)
	defer waitCancel()
	select {
	case <-doneCh:
	case <-waitCtx.Done():
		if ctx.Err() != nil {
			ec.log.Info("[EL] finalize cancelled mid-wait (race already won)")
		} else {
			ec.log.Warn("[EL] commit-wait timeout — no committed_transcript arrived")
			ec.recMu.Lock()
			ec.dropped = true
			ec.recMu.Unlock()
		}
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
		ec.log.Info("[EL] transcription", "post_release", postRelease.Round(time.Millisecond), "session", session.Round(time.Millisecond), "text", text)
	} else {
		ec.log.Info("[EL] no speech detected", "session", session.Round(time.Millisecond))
	}

	ec.close()
	return text
}

// wasDropped reports whether the WebSocket died before producing a clean
// committed_transcript for the active recording. Used by raceTranscribe to
// prefer REST fallbacks (which read the full local PCM buffer) over a
// partial streaming transcript.
func (ec *elConn) wasDropped() bool {
	ec.recMu.Lock()
	defer ec.recMu.Unlock()
	return ec.dropped
}
