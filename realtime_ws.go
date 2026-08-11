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
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// rtDeltaEvent carries a partial or final transcript from the OpenAI Realtime API.
type rtDeltaEvent struct {
	kind string // "delta" | "completed" | "error"
	text string
	err  error
}

// rtSession manages a single per-recording WebSocket session to the OpenAI
// Realtime transcription API. Created on key-press, closed on key-release.
type rtSession struct {
	conn   *websocket.Conn
	events chan rtDeltaEvent
	apiKey string
	model  string
	lang   string
	log    *slog.Logger

	// parts collects per-utterance transcripts from conversation.item.done
	// events — the newer transcribe models (gpt-live-transcribe) deliver text
	// per VAD segment instead of one input_audio_transcription.completed
	// event. Only touched from the readPump goroutine.
	parts     []string
	committed atomic.Bool

	// Async-connect state: the recorder starts immediately on key-press while
	// the socket dials in the background (same pattern as the Deepgram and
	// ElevenLabs clients). Chunks arriving before the socket is ready are
	// buffered under mu and flushed in order on connect. mu also serializes
	// all socket writes.
	mu      sync.Mutex
	pending [][]byte
	ready   bool
	failed  bool
}

func newRTSession(apiKey, model, lang string, log *slog.Logger) *rtSession {
	return &rtSession{
		apiKey: apiKey,
		model:  model,
		lang:   lang,
		events: make(chan rtDeltaEvent, 64),
		log:    log,
	}
}

// connect dials the WebSocket, sends session config, drains setup events,
// and starts the read pump in a goroutine. Returns error if dial or session
// config fails — in that case events chan is never used.
func (s *rtSession) connect(ctx context.Context) error {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	headers := http.Header{
		"Authorization": {"Bearer " + s.apiKey},
	}

	s.log.Info("[RT-WS] opening session", "model", s.model, "url", realtimeURL)
	conn, resp, err := dialer.DialContext(ctx, realtimeURL, headers)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		return fmt.Errorf("dial failed (status %d): %w", code, err)
	}
	s.conn = conn

	prompt := whisperPrompt
	if len(prompt) > 1024 {
		cut := strings.LastIndex(prompt[:1024], " ")
		if cut < 512 {
			cut = 1024
		}
		prompt = prompt[:cut]
	}

	sessionUpdate := map[string]interface{}{
		"type": "session.update",
		"session": map[string]interface{}{
			"type": "transcription",
			"audio": map[string]interface{}{
				"input": map[string]interface{}{
					// 24 kHz is the minimum the gpt-live-transcribe model
					// accepts; the 16 kHz recorder output is upsampled in
					// sendChunk to match.
					"format": map[string]interface{}{
						"type": "audio/pcm",
						"rate": 24000,
					},
					"transcription": map[string]interface{}{
						"model":    s.model,
						"language": s.lang,
						"prompt":   prompt,
					},
					"turn_detection": nil,
				},
			},
		},
	}
	if err := writeJSON(conn, sessionUpdate); err != nil {
		conn.Close()
		return fmt.Errorf("session config: %w", err)
	}
	s.log.Info("[RT-WS] session.update sent")

	// Drain setup events (session.created, session.updated) before returning.
	if err := s.drainSetup(ctx); err != nil {
		conn.Close()
		return fmt.Errorf("drain setup: %w", err)
	}

	go s.readPump(ctx)
	return nil
}

// drainSetup reads and discards setup phase events until session.updated or timeout.
func (s *rtSession) drainSetup(ctx context.Context) error {
	s.conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	defer s.conn.SetReadDeadline(time.Time{})
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, msg, err := s.conn.ReadMessage()
		if err != nil {
			return nil // timeout = no more setup events
		}
		var ev struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(msg, &ev) != nil {
			continue
		}
		s.log.Info("[RT-WS] setup event", "type", ev.Type)
		if ev.Type == "error" {
			// A rejected session config is fatal — treating it as success
			// used to leave the session mute (e.g. the 16 kHz rate rejection).
			s.log.Error("[RT-WS] session config rejected", "raw", string(msg))
			return fmt.Errorf("session config rejected: %s", msg)
		}
		if ev.Type == "session.updated" {
			return nil
		}
	}
}

// readPump runs in a goroutine, reading WebSocket messages and forwarding
// transcript events to s.events. Exits when the connection closes or ctx cancels.
func (s *rtSession) readPump(ctx context.Context) {
	defer close(s.events)
	for {
		if ctx.Err() != nil {
			return
		}
		_, msg, err := s.conn.ReadMessage()
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("[RT-WS] read error", "err", err)
				select {
				case s.events <- rtDeltaEvent{kind: "error", err: err}:
				default:
				}
			}
			return
		}

		var ev struct {
			Type       string `json:"type"`
			Delta      string `json:"delta"`
			Transcript string `json:"transcript"`
			Item       struct {
				Content []struct {
					Transcript string `json:"transcript"`
				} `json:"content"`
			} `json:"item"`
		}
		if json.Unmarshal(msg, &ev) != nil {
			continue
		}

		// emitCompleted sends the joined per-segment transcript as the final
		// text. Used by the newer per-item protocol (gpt-live-transcribe).
		emitCompleted := func() bool {
			t := strings.TrimSpace(strings.Join(s.parts, " "))
			if t == "" {
				return true
			}
			s.log.Info("[RT-WS] completed (items)", "text", t)
			select {
			case s.events <- rtDeltaEvent{kind: "completed", text: t}:
			case <-ctx.Done():
				return false
			}
			return true
		}

		switch ev.Type {
		case "conversation.item.done":
			var seg []string
			for _, c := range ev.Item.Content {
				if strings.TrimSpace(c.Transcript) != "" {
					seg = append(seg, strings.TrimSpace(c.Transcript))
				}
			}
			if len(seg) > 0 {
				s.parts = append(s.parts, strings.Join(seg, " "))
			}
			if s.committed.Load() {
				if !emitCompleted() {
					return
				}
			}

		case "input_audio_buffer.committed":
			// After commit, whatever segments exist may already be the full
			// transcript (the trailing item.done, if any, re-emits a longer
			// one — stopRTSession keeps the longest).
			if s.committed.Load() {
				if !emitCompleted() {
					return
				}
			}
		case "conversation.item.input_audio_transcription.delta":
			text := ev.Delta
			if len(text) > 80 {
				s.log.Info("[RT-WS] delta", "text", text[:80]+"...")
			} else {
				s.log.Info("[RT-WS] delta", "text", text)
			}
			select {
			case s.events <- rtDeltaEvent{kind: "delta", text: text}:
			case <-ctx.Done():
				return
			}

		case "conversation.item.input_audio_transcription.completed":
			t := strings.TrimSpace(ev.Transcript)
			s.log.Info("[RT-WS] completed", "text", t)
			select {
			case s.events <- rtDeltaEvent{kind: "completed", text: t}:
			case <-ctx.Done():
				return
			}
			return

		case "error":
			s.log.Error("[RT-WS] server error", "raw", string(msg))
			select {
			case s.events <- rtDeltaEvent{kind: "error", err: fmt.Errorf("server error: %s", msg)}:
			default:
			}
			return

		case "session.created", "session.updated",
			"input_audio_buffer.cleared", "input_audio_buffer.speech_started",
			"input_audio_buffer.speech_stopped",
			"conversation.item.created", "conversation.item.added",
			"response.created",
			"response.done", "response.output_item.added",
			"response.output_item.done":
			// informational

		default:
			s.log.Info("[RT-WS] unhandled event", "type", ev.Type)
		}
	}
}

// connectAsync dials and configures the session in the background so the
// recorder can start capturing immediately. On success it flushes any chunks
// buffered meanwhile; on failure it marks the session failed — the recorder's
// full local buffer still exists, so onRelease falls back to a batch backend.
func (s *rtSession) connectAsync(ctx context.Context) {
	go func() {
		if err := s.connect(ctx); err != nil {
			s.log.Error("[RT-WS] async connect failed", "err", err)
			s.mu.Lock()
			s.failed = true
			s.pending = nil
			s.mu.Unlock()
			close(s.events) // readPump never started; unblock the event drainer
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, chunk := range s.pending {
			if err := writeJSON(s.conn, map[string]interface{}{
				"type":  "input_audio_buffer.append",
				"audio": base64.StdEncoding.EncodeToString(chunk),
			}); err != nil {
				s.log.Warn("[RT-WS] flush of buffered audio failed", "err", err)
				s.failed = true
				s.pending = nil
				return
			}
		}
		s.log.Info("[RT-WS] connected, buffered audio flushed", "chunks", len(s.pending))
		s.pending = nil
		s.ready = true
	}()
}

// resample16to24 linearly interpolates 16 kHz mono s16le PCM to 24 kHz —
// the realtime API's minimum input rate.
func resample16to24(pcm []byte) []byte {
	n := len(pcm) / 2
	if n < 2 {
		return append([]byte(nil), pcm...)
	}
	in := make([]int16, n)
	for i := 0; i < n; i++ {
		in[i] = int16(uint16(pcm[2*i]) | uint16(pcm[2*i+1])<<8)
	}
	m := n * 3 / 2
	out := make([]byte, m*2)
	for k := 0; k < m; k++ {
		pos := float64(k) * 2 / 3
		i := int(pos)
		if i >= n-1 {
			i = n - 2
		}
		frac := pos - float64(i)
		s := int16(float64(in[i]) + (float64(in[i+1])-float64(in[i]))*frac)
		out[2*k] = byte(uint16(s))
		out[2*k+1] = byte(uint16(s) >> 8)
	}
	return out
}

// sendChunk forwards a PCM chunk to the socket, or buffers it while the
// async connect is still in flight. Never fails the recording: on a failed
// session the chunk is simply dropped here — the recorder keeps the full
// audio locally for the batch fallback.
func (s *rtSession) sendChunk(pcm []byte) error {
	data := resample16to24(pcm) // fresh slice, safe to retain
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed {
		return nil
	}
	if !s.ready {
		s.pending = append(s.pending, data)
		return nil
	}
	return writeJSON(s.conn, map[string]interface{}{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString(data),
	})
}

// commit signals end of utterance. If the async connect is still in flight it
// waits briefly — committing before the socket is ready would lose the take.
func (s *rtSession) commit() error {
	s.committed.Store(true)
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.mu.Lock()
		ready, failed := s.ready, s.failed
		s.mu.Unlock()
		if failed {
			return fmt.Errorf("session failed before commit")
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("connect still not ready at commit")
		}
		time.Sleep(25 * time.Millisecond)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeJSON(s.conn, map[string]interface{}{"type": "input_audio_buffer.commit"})
}

// close sends a WebSocket close frame and closes the connection.
func (s *rtSession) close() {
	if s.conn == nil {
		return
	}
	s.conn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	s.conn.Close()
	s.conn = nil
	s.log.Info("[RT-WS] session closed")
}

// ── Live-typing coordinator ────────────────────────────────────────

// rtTyper tracks what has been typed so far during a live streaming session.
// Delta events carry incremental fragments — appendDelta types them directly.
// The completed event may correct earlier words — correct uses the longest-common-prefix
// algorithm to backspace over the divergence and type the corrected suffix.
type rtTyper struct {
	typed      string
	targetHwnd uintptr
	log        *slog.Logger
}

func newRTTyper(targetHwnd uintptr, log *slog.Logger) *rtTyper {
	return &rtTyper{targetHwnd: targetHwnd, log: log}
}

// appendDelta types an incremental text fragment and records it.
func (t *rtTyper) appendDelta(fragment string) {
	if fragment == "" {
		return
	}
	t.log.Info("[RT-WS] typer: append delta", "len", len([]rune(fragment)))
	typeRunes([]rune(fragment), t.targetHwnd, t.log)
	t.typed += fragment
}

// correct applies a new authoritative full text, backspacing over any portion
// that diverges from what was already typed, then typing the correction.
// Used when the completed event differs from the accumulated deltas.
func (t *rtTyper) correct(authoritative string) {
	prefixLen := 0
	tyRunes := []rune(t.typed)
	newRunes := []rune(authoritative)
	for prefixLen < len(tyRunes) && prefixLen < len(newRunes) && tyRunes[prefixLen] == newRunes[prefixLen] {
		prefixLen++
	}

	backspaces := len(tyRunes) - prefixLen
	suffix := string(newRunes[prefixLen:])

	if backspaces > 0 {
		t.log.Info("[RT-WS] typer: correction backspace", "count", backspaces)
		sendBackspaces(backspaces, t.targetHwnd, t.log)
	}
	if suffix != "" {
		t.log.Info("[RT-WS] typer: correction suffix", "len", len([]rune(suffix)))
		typeRunes([]rune(suffix), t.targetHwnd, t.log)
	}

	t.typed = authoritative
}

// ── sttService integration helpers ────────────────────────────────

// startRTSession opens a new WebSocket session and stores it on the service.
// apiKey must be an OpenAI key — the active backend's key may differ.
// Called from onPress when StreamingMode is on or the whisper_live backend is
// active. liveType=false streams audio during the hold but types nothing until
// release (whisper_live); liveType=true also types deltas as they arrive.
func (s *sttService) startRTSession(ctx context.Context, apiKey string, liveType bool) {
	model := appConfig.RealtimeModel
	if model == "" {
		model = "gpt-4o-mini-transcribe"
	}
	sess := newRTSession(apiKey, model, s.lang, s.log)
	s.rtSession = sess
	if liveType {
		s.rtTyper = newRTTyper(s.targetHwnd, s.log)
	} else {
		s.rtTyper = nil
	}
	typer := s.rtTyper

	// Dial in the background — the recorder starts immediately and early
	// chunks are buffered inside the session, so the handshake costs the
	// user nothing and no audio is lost.
	sess.connectAsync(ctx)

	// Drain events from the session and apply typing in a goroutine.
	// Stores the final text into s.rtFinalText so onRelease can log it.
	go func() {
		for ev := range sess.events {
			switch ev.kind {
			case "delta":
				// Delta events carry incremental text fragments (not cumulative).
				// Append directly — no backspacing needed for normal flow.
				if typer != nil {
					typer.appendDelta(ev.text)
				}
			case "completed":
				// The completed event has the authoritative final transcript.
				// It may correct words that the mid-stream deltas got wrong.
				s.rtMu.Lock()
				s.rtFinalText = ev.text
				s.rtMu.Unlock()
			case "error":
				s.log.Error("[RT-WS] event error", "err", ev.err)
			}
		}
		s.log.Info("[RT-WS] event pump exited")
	}()
}

// stopRTSession commits the audio buffer, waits briefly for the final
// transcript event, then closes the session.
func (s *sttService) stopRTSession() string {
	sess := s.rtSession
	s.rtSession = nil
	typer := s.rtTyper
	s.rtTyper = nil

	if sess == nil {
		return ""
	}

	if err := sess.commit(); err != nil {
		// Session never became usable — no transcript is coming. Return
		// empty so the caller's batch fallback transcribes the local buffer.
		s.log.Warn("[RT-WS] commit failed", "err", err)
		sess.close()
		if typer != nil {
			return typer.typed
		}
		return ""
	}

	// Give the read pump up to 4 seconds to deliver the completed event.
	// The per-item protocol can emit the transcript twice in quick
	// succession (on committed, then again when a trailing segment closes),
	// so once text arrives, wait a short settle window and keep the longest.
	var best string
	bestAt := time.Time{}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		s.rtMu.Lock()
		txt := s.rtFinalText
		s.rtFinalText = ""
		s.rtMu.Unlock()
		if txt != "" && len(txt) >= len(best) {
			best = txt
			bestAt = time.Now()
		}
		if best != "" && time.Since(bestAt) > 250*time.Millisecond {
			break
		}
		if sess.conn == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if best != "" || sess.conn == nil {
		sess.close()
		if typer != nil && best != "" && best != typer.typed {
			typer.correct(best)
		}
		return best
	}

	s.log.Warn("[RT-WS] timed out waiting for completed event, using typed text")
	sess.close()
	if typer != nil {
		return typer.typed
	}
	return ""
}
