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
	"time"
	"unsafe"

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
	conn    *websocket.Conn
	events  chan rtDeltaEvent
	apiKey  string
	model   string
	lang    string
	log     *slog.Logger
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
					"format": map[string]interface{}{
						"type": "audio/pcm",
						"rate": 16000,
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
		if ev.Type == "session.updated" || ev.Type == "error" {
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
		}
		if json.Unmarshal(msg, &ev) != nil {
			continue
		}

		switch ev.Type {
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
			"input_audio_buffer.committed", "input_audio_buffer.cleared",
			"conversation.item.created", "response.created",
			"response.done", "response.output_item.added",
			"response.output_item.done":
			// informational

		default:
			s.log.Info("[RT-WS] unhandled event", "type", ev.Type)
		}
	}
}

// sendChunk encodes a PCM chunk as base64 and sends it as input_audio_buffer.append.
func (s *rtSession) sendChunk(pcm []byte) error {
	encoded := base64.StdEncoding.EncodeToString(pcm)
	return writeJSON(s.conn, map[string]interface{}{
		"type":  "input_audio_buffer.append",
		"audio": encoded,
	})
}

// commit sends input_audio_buffer.commit to signal end of utterance.
func (s *rtSession) commit() error {
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

// sendBackspaces sends n backspace key events to the target window.
func sendBackspaces(n int, targetHwnd uintptr, log *slog.Logger) {
	if n <= 0 {
		return
	}
	const vkBack = 0x08
	inputs := make([]kbInput, 0, n*2)
	for i := 0; i < n; i++ {
		inputs = append(inputs,
			kbInput{typ: inputKbd, vk: vkBack, flags: 0},
			kbInput{typ: inputKbd, vk: vkBack, flags: kfKeyup},
		)
	}
	if targetHwnd != 0 {
		pSetForegroundWindow.Call(targetHwnd)
	}
	ret, _, _ := pSendInput.Call(uintptr(len(inputs)), uintptr(unsafe.Pointer(&inputs[0])), unsafe.Sizeof(inputs[0]))
	if ret == 0 {
		log.Error("[RT-WS] sendBackspaces: SendInput failed", "n", n)
	}
}

// typeRunes sends unicode key events for a slice of runes. Reuses the same
// pattern as typeText in whisper.go but without the RightAlt-release wait
// (we're mid-recording, the key is still held).
func typeRunes(runes []rune, targetHwnd uintptr, log *slog.Logger) {
	if len(runes) == 0 {
		return
	}
	inputs := make([]kbInput, 0, len(runes)*2)
	for _, ch := range runes {
		inputs = append(inputs,
			kbInput{typ: inputKbd, scan: uint16(ch), flags: kfUnicode},
			kbInput{typ: inputKbd, scan: uint16(ch), flags: kfUnicode | kfKeyup},
		)
	}
	if targetHwnd != 0 {
		pSetForegroundWindow.Call(targetHwnd)
	}
	ret, _, _ := pSendInput.Call(uintptr(len(inputs)), uintptr(unsafe.Pointer(&inputs[0])), unsafe.Sizeof(inputs[0]))
	if ret == 0 {
		log.Error("[RT-WS] typeRunes: SendInput failed", "runes", len(runes))
	}
}

// ── sttService integration helpers ────────────────────────────────

// startRTSession opens a new WebSocket session and stores it on the service.
// apiKey must be an OpenAI key — the active backend's key may differ.
// Called from onPress when StreamingMode is on.
func (s *sttService) startRTSession(ctx context.Context, apiKey string) {
	model := appConfig.RealtimeModel
	if model == "" {
		model = "gpt-4o-mini-transcribe"
	}
	sess := newRTSession(apiKey, model, s.lang, s.log)
	if err := sess.connect(ctx); err != nil {
		s.log.Error("[RT-WS] failed to open session, will buffer and fall back", "err", err)
		s.rtSession = nil
		return
	}
	s.rtSession = sess
	s.rtTyper = newRTTyper(s.targetHwnd, s.log)

	// Drain events from the session and apply typing in a goroutine.
	// Stores the final text into s.rtFinalText so onRelease can log it.
	go func() {
		for ev := range sess.events {
			switch ev.kind {
			case "delta":
				// Delta events carry incremental text fragments (not cumulative).
				// Append directly — no backspacing needed for normal flow.
				s.rtTyper.appendDelta(ev.text)
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
		s.log.Warn("[RT-WS] commit failed", "err", err)
	}

	// Give the read pump up to 4 seconds to deliver the completed event.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		s.rtMu.Lock()
		txt := s.rtFinalText
		s.rtFinalText = ""
		s.rtMu.Unlock()
		if txt != "" || sess.conn == nil {
			sess.close()
			if typer != nil && txt != "" && txt != typer.typed {
				typer.correct(txt)
			}
			return txt
		}
		time.Sleep(50 * time.Millisecond)
	}

	s.log.Warn("[RT-WS] timed out waiting for completed event, using typed text")
	sess.close()
	if typer != nil {
		return typer.typed
	}
	return ""
}
