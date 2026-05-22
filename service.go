//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/energye/systray"
)

// ── Post-processing replacements for commonly misheard terms ──────

// Case-insensitive replacements applied to every transcription result.
// Populated from appConfig.Replacements at startup via loadReplacements().
var postProcessReplacements []struct{ from, to string }

func postProcess(text string) string {
	for _, r := range postProcessReplacements {
		// Case-insensitive replace
		lower := strings.ToLower(text)
		fromLower := strings.ToLower(r.from)
		idx := 0
		for {
			pos := strings.Index(lower[idx:], fromLower)
			if pos == -1 {
				break
			}
			pos += idx
			text = text[:pos] + r.to + text[pos+len(r.from):]
			lower = strings.ToLower(text)
			idx = pos + len(r.to)
		}
	}
	return text
}

// ── Tray state ─────────────────────────────────────────────────────

type trayState int

const (
	stateIdle trayState = iota
	stateListening
	stateTranscribing
)

// makeICO generates a 16x16 32-bit ICO with a filled circle.
func makeICO(r, g, b, a byte) []byte {
	const size = 16
	var buf bytes.Buffer

	binary.Write(&buf, binary.LittleEndian, uint16(0))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint16(1))

	pixelData := size * size * 4
	andMask := size * 4
	imgSize := uint32(40 + pixelData + andMask)
	buf.WriteByte(size)
	buf.WriteByte(size)
	buf.WriteByte(0)
	buf.WriteByte(0)
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint16(32))
	binary.Write(&buf, binary.LittleEndian, imgSize)
	binary.Write(&buf, binary.LittleEndian, uint32(22))

	binary.Write(&buf, binary.LittleEndian, uint32(40))
	binary.Write(&buf, binary.LittleEndian, int32(size))
	binary.Write(&buf, binary.LittleEndian, int32(size*2))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint16(32))
	binary.Write(&buf, binary.LittleEndian, uint32(0))
	binary.Write(&buf, binary.LittleEndian, uint32(pixelData+andMask))
	binary.Write(&buf, binary.LittleEndian, int32(0))
	binary.Write(&buf, binary.LittleEndian, int32(0))
	binary.Write(&buf, binary.LittleEndian, uint32(0))
	binary.Write(&buf, binary.LittleEndian, uint32(0))

	cx, cy := float64(size-1)/2, float64(size-1)/2
	radius := float64(size)/2 - 1
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx := float64(x) - cx
			dy := float64(y) - cy
			dist := math.Sqrt(dx*dx + dy*dy)
			if dist <= radius-0.5 {
				buf.Write([]byte{b, g, r, a})
			} else if dist <= radius+0.5 {
				aa := byte(float64(a) * (radius + 0.5 - dist))
				buf.Write([]byte{b, g, r, aa})
			} else {
				buf.Write([]byte{0, 0, 0, 0})
			}
		}
	}

	buf.Write(make([]byte, andMask))
	return buf.Bytes()
}

// ── STT Service ────────────────────────────────────────────────────

type sttService struct {
	backend    string
	lang       string
	apiKey     string
	rec        *recorder
	dgc        *dgConn
	elc        *elConn
	log        *slog.Logger
	onState    func(trayState)
	overlay    *waveOverlay
	recT0      time.Time
	targetHwnd uintptr // foreground window when recording started
}

func newSTTService(backend, lang string, log *slog.Logger) *sttService {
	s := &sttService{backend: backend, lang: lang, rec: newRecorder(log), log: log}
	switch backend {
	case "api":
		s.apiKey = readEnvKey("OPENAI_API_KEY")
		if s.apiKey == "" {
			log.Error("[SVC] OPENAI_API_KEY not found")
		}
		log.Info("[SVC] OpenAI client ready")
	case "whisper_stream":
		s.apiKey = readEnvKey("OPENAI_API_KEY")
		if s.apiKey == "" {
			log.Error("[SVC] OPENAI_API_KEY not found")
		}
		log.Info("[SVC] Whisper streaming client ready")
	case "whisper_realtime":
		s.apiKey = readEnvKey("OPENAI_API_KEY")
		if s.apiKey == "" {
			log.Error("[SVC] OPENAI_API_KEY not found")
		}
		log.Info("[SVC] Whisper Realtime client ready")
	case "deepgram":
		s.apiKey = readEnvKey("DEEPGRAM_API_KEY")
		if s.apiKey == "" {
			log.Error("[SVC] DEEPGRAM_API_KEY not found")
		}
		s.dgc = newDGConn(s.apiKey, lang, log)
		log.Info("[SVC] Deepgram client ready (on-demand connection)")
	case "elevenlabs":
		s.apiKey = readEnvKey("ELEVENLABS_API_KEY")
		if s.apiKey == "" {
			log.Error("[SVC] ELEVENLABS_API_KEY not found")
		}
		s.elc = newELConn(s.apiKey, lang, log)
		log.Info("[SVC] ElevenLabs client ready (on-demand connection)")
	case "elevenlabs_batch":
		s.apiKey = readEnvKey("ELEVENLABS_API_KEY")
		if s.apiKey == "" {
			log.Error("[SVC] ELEVENLABS_API_KEY not found")
		}
		log.Info("[SVC] ElevenLabs batch client ready (REST upload with keyterms)")
	}
	log.Info("[SVC] STT Service initialized", "backend", backend, "language", lang)
	return s
}

func (s *sttService) switchBackend(backend string) {
	if backend == s.backend {
		return
	}
	s.log.Info("[SVC] Switching backend", "from", s.backend, "to", backend)

	// Close existing connections if switching away
	if s.backend == "deepgram" && s.dgc != nil {
		s.dgc.close()
		s.dgc = nil
	}
	if s.backend == "elevenlabs" && s.elc != nil {
		s.elc.close()
		s.elc = nil
	}

	s.backend = backend
	switch backend {
	case "api":
		s.apiKey = readEnvKey("OPENAI_API_KEY")
		if s.apiKey == "" {
			s.log.Error("[SVC] OPENAI_API_KEY not found")
		}
		s.log.Info("[SVC] Switched to Whisper")
	case "whisper_stream":
		s.apiKey = readEnvKey("OPENAI_API_KEY")
		if s.apiKey == "" {
			s.log.Error("[SVC] OPENAI_API_KEY not found")
		}
		s.log.Info("[SVC] Switched to Whisper Streaming")
	case "whisper_realtime":
		s.apiKey = readEnvKey("OPENAI_API_KEY")
		if s.apiKey == "" {
			s.log.Error("[SVC] OPENAI_API_KEY not found")
		}
		s.log.Info("[SVC] Switched to Whisper Realtime")
	case "deepgram":
		s.apiKey = readEnvKey("DEEPGRAM_API_KEY")
		if s.apiKey == "" {
			s.log.Error("[SVC] DEEPGRAM_API_KEY not found")
		}
		s.dgc = newDGConn(s.apiKey, s.lang, s.log)
		s.log.Info("[SVC] Switched to Deepgram (on-demand connection)")
	case "elevenlabs":
		s.apiKey = readEnvKey("ELEVENLABS_API_KEY")
		if s.apiKey == "" {
			s.log.Error("[SVC] ELEVENLABS_API_KEY not found")
		}
		s.elc = newELConn(s.apiKey, s.lang, s.log)
		s.log.Info("[SVC] Switched to ElevenLabs (on-demand connection)")
	case "elevenlabs_batch":
		s.apiKey = readEnvKey("ELEVENLABS_API_KEY")
		if s.apiKey == "" {
			s.log.Error("[SVC] ELEVENLABS_API_KEY not found")
		}
		s.log.Info("[SVC] Switched to ElevenLabs Batch (REST upload with keyterms)")
	}
}

func (s *sttService) onPress() {
	// Save the foreground window before recording starts so we can
	// restore it before typing (overlay or other windows may steal focus)
	hwnd, _, _ := pGetForegroundWindow.Call()
	s.targetHwnd = hwnd
	s.log.Info("[KEY] onPress: captured foreground window", "hwnd", fmt.Sprintf("0x%X", hwnd))

	if s.onState != nil {
		s.onState(stateListening)
	}
	if s.overlay != nil {
		s.overlay.show()
	}

	s.recT0 = time.Now()

	switch s.backend {
	case "deepgram":
		s.dgc.startRecording()
		s.rec.onChunk = func(data []byte) {
			s.rec.allData = append(s.rec.allData, data...)
			if s.overlay != nil {
				s.overlay.pushAudio(data)
			}
			s.dgc.send(data)
		}
	case "elevenlabs":
		s.elc.startRecording()
		s.rec.onChunk = func(data []byte) {
			s.rec.allData = append(s.rec.allData, data...)
			if s.overlay != nil {
				s.overlay.pushAudio(data)
			}
			s.elc.send(data)
		}
	default: // "api", "whisper_stream", "whisper_realtime" — buffer full audio then POST/connect
		s.rec.onChunk = func(data []byte) {
			s.rec.allData = append(s.rec.allData, data...)
			if s.overlay != nil {
				s.overlay.pushAudio(data)
			}
		}
	}

	if err := s.rec.start(); err != nil {
		s.log.Error("[KEY] Recording failed to start", "err", err)
		return
	}
}

func (s *sttService) onRelease() {
	holdDuration := time.Since(s.recT0)
	s.log.Info("[KEY] onRelease: key released", "hold_duration", holdDuration.Round(time.Millisecond))
	if s.overlay != nil {
		s.overlay.showTranscribing()
	}
	targetHwnd := s.targetHwnd
	go func() {
		defer func() {
			if p := recover(); p != nil {
				s.log.Error("[KEY] onRelease: panic recovered", "panic", fmt.Sprintf("%v", p))
			}
			if s.overlay != nil {
				s.overlay.hide()
			}
		}()
		if s.onState != nil {
			s.onState(stateTranscribing)
			defer func() { s.onState(stateIdle) }()
		}
		_, totalBytes := s.rec.stop()
		duration := float64(totalBytes) / float64(avgBytesPerSec)

		if duration < 0.3 {
			s.log.Info("[KEY] Too short, ignoring", "backend", s.backend, "duration", fmt.Sprintf("%.2fs", duration))
			return
		}

		// Save every recording to debug-audio/ for diagnosis
		debugFile := saveDebugAudio(s.rec.allData, s.log)
		pcm := s.rec.allData

		transcribeStart := time.Now()
		usedBackend := s.backend
		var text string
		var transcribeErr error

		switch s.backend {
		case "api":
			// Whisper REST — no racing, retry transient failures
			cfg := defaultRetryConfig()
			cfg.onRetry = func() { closeIdleConns(whisperHTTPClient) }
			res := withRetry(context.Background(), cfg, "whisper", s.log,
				func(ctx context.Context) (string, error) {
					return transcribeWhisper(ctx, pcm, s.apiKey, s.lang, s.log)
				})
			text, transcribeErr = res.text, res.err
		case "whisper_stream":
			// Whisper SSE streaming — no racing, retry transient failures
			cfg := defaultRetryConfig()
			cfg.onRetry = func() { closeIdleConns(whisperStreamHTTPClient) }
			res := withRetry(context.Background(), cfg, "whisper_stream", s.log,
				func(ctx context.Context) (string, error) {
					return transcribeWhisperStream(ctx, pcm, s.apiKey, s.lang, s.log)
				})
			text, transcribeErr = res.text, res.err
		case "whisper_realtime":
			// Whisper Realtime WebSocket — no racing, retry transient failures
			cfg := defaultRetryConfig()
			res := withRetry(context.Background(), cfg, "whisper_realtime", s.log,
				func(ctx context.Context) (string, error) {
					return transcribeWhisperRealtime(ctx, pcm, s.apiKey, s.lang, s.log)
				})
			text, transcribeErr = res.text, res.err
		case "elevenlabs_batch":
			// ElevenLabs REST upload — full keyterms biasing, no racing
			cfg := defaultRetryConfig()
			cfg.onRetry = func() { closeIdleConns(elevenLabsRESTHTTPClient) }
			res := withRetry(context.Background(), cfg, "elevenlabs_batch", s.log,
				func(ctx context.Context) (string, error) {
					return transcribeElevenLabsREST(ctx, pcm, s.apiKey, s.lang, s.log)
				})
			text, transcribeErr = res.text, res.err
		case "deepgram", "elevenlabs":
			// Race: streaming backend vs REST fallbacks, each with its own retry loop
			text, usedBackend, transcribeErr = s.raceTranscribe(pcm, duration)
		}

		transcribeElapsed := time.Since(transcribeStart)
		sessionElapsed := time.Since(s.recT0)

		if transcribeErr != nil {
			s.log.Error("[STT] result",
				"backend", usedBackend,
				"language", s.lang,
				"duration", fmt.Sprintf("%.1fs", duration),
				"transcribe_time", transcribeElapsed.Round(time.Millisecond),
				"session", sessionElapsed.Round(time.Millisecond),
				"error", transcribeErr,
			)
			return
		}

		// Unified log line for every transcription — grep "[STT] result" to analyze
		s.log.Info("[STT] result",
			"backend", usedBackend,
			"language", s.lang,
			"duration", fmt.Sprintf("%.1fs", duration),
			"transcribe_time", transcribeElapsed.Round(time.Millisecond),
			"session", sessionElapsed.Round(time.Millisecond),
			"audio_file", debugFile,
			"text", text,
		)

		if text != "" {
			text = postProcess(text)
			typeText(text, targetHwnd, s.log)
		}

		// Background Whisper comparison — always run regardless of primary backend
		// to detect transcription differences and build keyword correction data
		if text != "" && usedBackend != "api" && !strings.Contains(usedBackend, "whisper") && len(pcm) > 0 {
			pcmCopy := make([]byte, len(pcm))
			copy(pcmCopy, pcm)
			primaryText := text
			primaryBackend := usedBackend
			audioFile := debugFile
			go s.compareWithWhisper(pcmCopy, primaryText, primaryBackend, audioFile, duration)
		}
	}()
}

// raceStreamingDeadline is how long we wait for a clean streaming result
// before firing REST fallbacks. If streaming delivers a non-dropped transcript
// within this window, we skip the REST calls entirely.
const raceStreamingDeadline = 2 * time.Second

// raceHardTimeout is the absolute cap on the entire race. If all backends are
// still retrying after this, we give up and save audio to disk.
const raceHardTimeout = 25 * time.Second

// raceTranscribe races the streaming backend (Deepgram/ElevenLabs WS) against
// two REST backends (Whisper, ElevenLabs Scribe REST), each with its own retry
// loop. Design:
//
//  1. All three backends start immediately.
//  2. If the STREAMING backend finishes cleanly within raceStreamingDeadline
//     (no connection drop), it wins — REST calls are cancelled and we return.
//  3. Otherwise, first REST backend to produce a non-empty transcript wins —
//     the other REST and the streaming finalize are cancelled.
//  4. If all three exhaust their retries with no usable transcript, the audio
//     is saved to failed-audio/ for later recovery.
//
// Each REST backend runs withRetry independently. A single backend failing
// does not affect the others. Cancellation propagates via context — losing
// backends abort in-flight HTTP calls promptly, no wasted bandwidth.
func (s *sttService) raceTranscribe(pcm []byte, duration float64) (text, usedBackend string, err error) {
	// Parent context bounds the whole race. cancel() is called on first win
	// OR on timeout to abort all remaining goroutines. Every backend must
	// honor this context for the cancellation to work.
	ctx, cancel := context.WithTimeout(context.Background(), raceHardTimeout)
	defer cancel()

	type result struct {
		text    string
		backend string
		err     error
		dropped bool // streaming-only: true if WS connection dropped mid-recording
	}

	// Buffered so late senders never block after a winner is chosen.
	// 3 slots = streaming + whisper + elevenlabs, at most one send per backend.
	ch := make(chan result, 3)

	// ── Backend 1: streaming (Deepgram or ElevenLabs WS) ────────────────
	go func() {
		var t string
		var dropped bool
		switch s.backend {
		case "deepgram":
			t = s.dgc.finalize(ctx, s.recT0)
			dropped = s.dgc.wasDropped()
		case "elevenlabs":
			t = s.elc.finalize(ctx, s.recT0)
			dropped = s.elc.wasDropped()
		}
		if dropped && t == "" {
			ch <- result{"", s.backend, fmt.Errorf("connection dropped"), true}
		} else {
			ch <- result{t, s.backend, nil, dropped}
		}
	}()

	// ── Backend 2: Whisper REST (retry loop) ────────────────────────────
	whisperKey := readEnvKey("OPENAI_API_KEY")
	if whisperKey != "" {
		whisperCfg := defaultRetryConfig()
		whisperCfg.onRetry = func() { closeIdleConns(whisperHTTPClient) }
		go func() {
			res := withRetry(ctx, whisperCfg, "whisper_rest", s.log,
				func(attemptCtx context.Context) (string, error) {
					return transcribeWhisper(attemptCtx, pcm, whisperKey, s.lang, s.log)
				})
			ch <- result{res.text, "whisper_rest", res.err, false}
		}()
	}

	// ── Backend 3: ElevenLabs REST (retry loop) ─────────────────────────
	elevenLabsKey := readEnvKey("ELEVENLABS_API_KEY")
	if elevenLabsKey != "" {
		elCfg := defaultRetryConfig()
		elCfg.onRetry = func() { closeIdleConns(elevenLabsRESTHTTPClient) }
		go func() {
			res := withRetry(ctx, elCfg, "elevenlabs_rest", s.log,
				func(attemptCtx context.Context) (string, error) {
					return transcribeElevenLabsREST(attemptCtx, pcm, elevenLabsKey, s.lang, s.log)
				})
			ch <- result{res.text, "elevenlabs_rest", res.err, false}
		}()
	}

	expected := 1 // streaming always runs
	if whisperKey != "" {
		expected++
	}
	if elevenLabsKey != "" {
		expected++
	}

	// ── Phase 1: give streaming raceStreamingDeadline for a CLEAN win ───
	var streamingResult *result
	deadline := time.NewTimer(raceStreamingDeadline)
	defer deadline.Stop()

	phase1Loop:
	for {
		select {
		case r := <-ch:
			if r.backend == s.backend {
				// Streaming finished
				if r.err == nil && r.text != "" && !r.dropped {
					// Clean streaming win — cancel REST backends
					s.log.Info("[RACE] streaming won race (clean)", "backend", r.backend, "text_len", len(r.text))
					cancel()
					return r.text, r.backend, nil
				}
				// Streaming either failed, dropped, or returned empty — keep its
				// partial result as a fallback but continue to phase 2.
				streamingResult = &r
				if r.dropped {
					s.log.Warn("[RACE] streaming dropped — waiting on REST for full audio",
						"backend", r.backend, "partial_text", r.text)
				} else if r.err != nil {
					s.log.Warn("[RACE] streaming failed — waiting on REST",
						"backend", r.backend, "err", r.err)
				} else {
					s.log.Warn("[RACE] streaming empty — waiting on REST",
						"backend", r.backend)
				}
				break phase1Loop
			}
			// A REST backend finished first (streaming still in flight).
			// If it succeeded, that's our winner — cancel the rest.
			if r.err == nil && r.text != "" {
				s.log.Info("[RACE] REST won race (first)", "backend", r.backend, "text_len", len(r.text))
				cancel()
				return r.text, r.backend, nil
			}
			// REST failed — note it and keep waiting. We'll collect it in phase 2.
			s.log.Warn("[RACE] REST finished with no result before streaming",
				"backend", r.backend, "err", r.err)
			// Push it back into the channel so phase 2 can pick it up? No —
			// simpler: record and continue.
			_ = r
		case <-deadline.C:
			s.log.Warn("[RACE] streaming deadline exceeded, proceeding with REST",
				"deadline", raceStreamingDeadline)
			break phase1Loop
		case <-ctx.Done():
			return "", s.backend, ctx.Err()
		}
	}

	// ── Phase 2: first successful REST wins, streaming is safety net ────
	// We've consumed at most one result from ch (the streaming one).
	// Wait for the remaining backends. First REST success → cancel + return.
	var lastErr error
	type candidate struct {
		text    string
		backend string
	}
	var candidates []candidate
	if streamingResult != nil && streamingResult.text != "" {
		// Include the (possibly partial/dropped) streaming result as a safety
		// net. Only used if all REST backends also fail.
		candidates = append(candidates, candidate{streamingResult.text, streamingResult.backend})
	}

	// How many more results to collect?
	remaining := expected
	if streamingResult != nil {
		remaining-- // streaming already consumed
	}

	for remaining > 0 {
		select {
		case r := <-ch:
			remaining--
			if r.err != nil {
				s.log.Warn("[RACE] backend failed", "backend", r.backend, "err", r.err)
				lastErr = r.err
				continue
			}
			if r.text == "" {
				s.log.Warn("[RACE] backend returned empty transcript", "backend", r.backend)
				continue
			}
			// REST success — first-REST-wins. Cancel everything else and return.
			if r.backend != s.backend {
				s.log.Info("[RACE] REST won race", "backend", r.backend, "text_len", len(r.text))
				cancel()
				return r.text, r.backend, nil
			}
			// Late streaming result — hold it as a candidate only
			candidates = append(candidates, candidate{r.text, r.backend})
			s.log.Info("[RACE] late streaming result collected", "backend", r.backend, "text_len", len(r.text))
		case <-ctx.Done():
			s.log.Error("[RACE] hard timeout exceeded", "timeout", raceHardTimeout)
			goto pickBest
		}
	}

pickBest:
	if len(candidates) == 0 {
		// Total failure — save audio for later recovery
		savedPath := saveAudioToDisk(pcm, s.log)
		if savedPath != "" {
			s.log.Error("[RACE] all backends failed — audio saved to disk",
				"path", savedPath, "duration", fmt.Sprintf("%.1fs", duration), "last_err", lastErr)
		}
		return "", s.backend, lastErr
	}

	// Only reached if ALL REST backends failed AND we have a streaming result.
	// Pick the longest transcript as a last resort.
	best := candidates[0]
	for _, c := range candidates[1:] {
		if len(c.text) > len(best.text) {
			best = c
		}
	}
	s.log.Info("[RACE] fell back to streaming/longest", "winner", best.backend, "winner_len", len(best.text))
	return best.text, best.backend, nil
}

func (s *sttService) run(ctx context.Context) {
	s.log.Info("[SVC] STT Service running — hold Right Alt to record, release to transcribe")

	pressed := false
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			s.log.Info("[SVC] STT Service stopped")
			return
		case <-tick.C:
			st, _, _ := pGetAsyncKey.Call(vkRMenu)
			down := int16(st) < 0
			if down && !pressed {
				pressed = true
				s.onPress()
			} else if !down && pressed {
				pressed = false
				s.onRelease()
			}
		}
	}
}

// mismatchEntry represents a transcription difference between primary backend and Whisper.
type mismatchEntry struct {
	Timestamp      string  `json:"timestamp"`
	PrimaryBackend string  `json:"primary_backend"`
	PrimaryText    string  `json:"primary_text"`
	WhisperText    string  `json:"whisper_text"`
	AudioFile      string  `json:"audio_file"`
	Duration       float64 `json:"duration_sec"`
}

// compareWithWhisper re-transcribes audio with Whisper and logs differences.
func (s *sttService) compareWithWhisper(pcm []byte, primaryText, primaryBackend, audioFile string, duration float64) {
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("[WH] compareWithWhisper: panic", "panic", fmt.Sprintf("%v", p))
		}
	}()

	whisperKey := readEnvKey("OPENAI_API_KEY")
	if whisperKey == "" {
		return
	}

	// Background comparison runs on a generous timeout; no retry needed —
	// if it fails, we just skip the mismatch log for this utterance.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	whisperText, err := transcribeWhisper(ctx, pcm, whisperKey, s.lang, s.log)
	if err != nil {
		s.log.Warn("[WH] Background comparison failed", "err", err)
		return
	}

	// Normalize for comparison: lowercase, trim
	normPrimary := strings.ToLower(strings.TrimSpace(primaryText))
	normWhisper := strings.ToLower(strings.TrimSpace(whisperText))

	// Remove trailing punctuation for comparison
	normPrimary = strings.TrimRight(normPrimary, ".!?,;:")
	normWhisper = strings.TrimRight(normWhisper, ".!?,;:")

	if normPrimary == normWhisper {
		s.log.Info("[WH] Background match", "audio_file", audioFile)
		return
	}

	s.log.Warn("[WH] Transcription mismatch detected",
		"primary_backend", primaryBackend,
		"primary_text", primaryText,
		"whisper_text", whisperText,
		"audio_file", audioFile,
	)

	entry := mismatchEntry{
		Timestamp:      time.Now().Format(time.RFC3339),
		PrimaryBackend: primaryBackend,
		PrimaryText:    primaryText,
		WhisperText:    whisperText,
		AudioFile:      audioFile,
		Duration:       duration,
	}
	appendMismatch(entry, s.log)
}

// appendMismatch appends a mismatch entry to mismatches.jsonl.
func appendMismatch(entry mismatchEntry, log *slog.Logger) {
	exe, _ := os.Executable()
	path := filepath.Join(filepath.Dir(exe), "mismatches.jsonl")

	data, err := json.Marshal(entry)
	if err != nil {
		log.Error("[SVC] Failed to marshal mismatch", "err", err)
		return
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Error("[SVC] Failed to open mismatches.jsonl", "err", err)
		return
	}
	defer f.Close()
	f.Write(append(data, '\n'))
}

// cleanupOldFiles removes debug audio files and mismatch entries older than 7 days.
// Called once on startup.
func cleanupOldFiles(log *slog.Logger) {
	exe, _ := os.Executable()
	baseDir := filepath.Dir(exe)
	cutoff := time.Now().AddDate(0, 0, -7)

	// Clean debug-audio/
	debugDir := filepath.Join(baseDir, "debug-audio")
	entries, err := os.ReadDir(debugDir)
	if err == nil {
		removed := 0
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				if err := os.Remove(filepath.Join(debugDir, e.Name())); err != nil {
					log.Error("[CLEANUP] failed to remove file", "path", filepath.Join(debugDir, e.Name()), "err", err)
				}
				removed++
			}
		}
		if removed > 0 {
			log.Info("[SVC] Cleaned up old debug audio files", "removed", removed)
		}
	}

	// Clean failed-audio/
	failedDir := filepath.Join(baseDir, "failed-audio")
	entries, err = os.ReadDir(failedDir)
	if err == nil {
		removed := 0
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				if err := os.Remove(filepath.Join(failedDir, e.Name())); err != nil {
					log.Error("[CLEANUP] failed to remove file", "path", filepath.Join(failedDir, e.Name()), "err", err)
				}
				removed++
			}
		}
		if removed > 0 {
			log.Info("[SVC] Cleaned up old failed audio files", "removed", removed)
		}
	}

	// Clean old entries from mismatches.jsonl
	mismatchPath := filepath.Join(baseDir, "mismatches.jsonl")
	data, err := os.ReadFile(mismatchPath)
	if err != nil {
		return // file doesn't exist yet, that's fine
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var kept []string
	for _, line := range lines {
		if line == "" {
			continue
		}
		var entry mismatchEntry
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		t, err := time.Parse(time.RFC3339, entry.Timestamp)
		if err != nil || t.After(cutoff) {
			kept = append(kept, line)
		}
	}

	if len(kept) < len(lines) {
		removed := len(lines) - len(kept)
		os.WriteFile(mismatchPath, []byte(strings.Join(kept, "\n")+"\n"), 0644)
		log.Info("[SVC] Cleaned up old mismatch entries", "removed", removed, "kept", len(kept))
	}
}

// saveAudioToDisk saves raw PCM audio as WAV to disk when all backends fail.
// Returns the file path, or empty string on error.
func saveAudioToDisk(pcm []byte, log *slog.Logger) string {
	exe, _ := os.Executable()
	dir := filepath.Join(filepath.Dir(exe), "failed-audio")
	os.MkdirAll(dir, 0755)

	filename := fmt.Sprintf("stt-%s.wav", time.Now().Format("2006-01-02T15-04-05"))
	path := filepath.Join(dir, filename)

	wav := pcmToWAV(pcm)
	if err := os.WriteFile(path, wav, 0644); err != nil {
		log.Error("[SVC] Failed to save audio to disk", "err", err)
		return ""
	}
	log.Info("[SVC] Audio saved to disk for later retry", "path", path, "size", len(wav))
	return path
}

// saveDebugAudio saves every recording to debug-audio/ for diagnosis.
// Returns just the filename (not full path) for compact logging.
func saveDebugAudio(pcm []byte, log *slog.Logger) string {
	exe, _ := os.Executable()
	dir := filepath.Join(filepath.Dir(exe), "debug-audio")
	os.MkdirAll(dir, 0755)

	filename := fmt.Sprintf("stt-%s.wav", time.Now().Format("2006-01-02T15-04-05.000"))
	path := filepath.Join(dir, filename)

	wav := pcmToWAV(pcm)
	if err := os.WriteFile(path, wav, 0644); err != nil {
		log.Error("[SVC] Failed to save debug audio", "err", err)
		return ""
	}
	return filename
}

// ── Tray setup ─────────────────────────────────────────────────────

func setupTray(svc *sttService, backend string, log *slog.Logger) {
	// Load custom icon for idle state, fall back to generated circle
	exe, _ := os.Executable()
	iconPath := filepath.Join(filepath.Dir(exe), "icon.ico")
	iconIdle, err := os.ReadFile(iconPath)
	if err != nil {
		log.Warn("[SVC] Could not load icon.ico, using fallback", "err", err)
		iconIdle = makeICO(128, 128, 128, 255)
	}
	iconListen := makeICO(76, 175, 80, 255)
	iconTranscribe := makeICO(255, 152, 0, 255)

	ctx, cancel := context.WithCancel(context.Background())

	// Start clipboard paste-path hotkey (Ctrl+Shift+V)
	go runClipboardHotkey(ctx, log)

	systray.Run(func() {
		systray.SetIcon(iconIdle)
		systray.SetTooltip("STT-Go: Idle")

		backendLabel := map[string]string{
			"deepgram":         "Deepgram Nova-3",
			"api":              "Whisper",
			"elevenlabs":       "ElevenLabs Scribe",
			"whisper_stream":   "Whisper (streaming)",
			"whisper_realtime": "Whisper (realtime)",
		}[backend]
		if backendLabel == "" {
			backendLabel = backend
		}
		mInfo := systray.AddMenuItem(fmt.Sprintf("STT-Go (%s)", backendLabel), "")
		mInfo.Disable()

		// Microphone submenu
		mMicMenu := systray.AddMenuItem("Microphone", "Select input device")
		mics := listMics()
		var micItems []*systray.MenuItem
		activeDeviceID := svc.rec.deviceID

		for _, mic := range mics {
			item := mMicMenu.AddSubMenuItem(mic.Name, "")
			if mic.ID == activeDeviceID {
				item.Check()
			}
			micID := mic.ID
			micName := mic.Name
			item.Click(func() {
				// Uncheck all, check selected
				for _, mi := range micItems {
					mi.Uncheck()
				}
				item.Check()
				svc.rec.setDeviceID(micID)
				log.Info("[CFG] Switched microphone", "device", micID, "name", micName)
				// Persist selection to config
				appConfig.MicDevice = micName
				if err := saveConfig(appConfig); err != nil {
					log.Error("[CFG] Failed to save mic preference", "err", err)
				}
			})
			micItems = append(micItems, item)
		}
		if len(mics) == 0 {
			noMic := mMicMenu.AddSubMenuItem("No microphones found", "")
			noMic.Disable()
		}

		// Backend submenu
		mBackendMenu := systray.AddMenuItem("Backend", "Select transcription backend")
		mDeepgram := mBackendMenu.AddSubMenuItem("Deepgram Nova-3", "")
		mElevenLabs := mBackendMenu.AddSubMenuItem("ElevenLabs Scribe (streaming)", "")
		mElevenLabsBatch := mBackendMenu.AddSubMenuItem("ElevenLabs Scribe (batch + keyterms)", "")
		mWhisper := mBackendMenu.AddSubMenuItem("Whisper (OpenAI)", "")
		mWhisperStream := mBackendMenu.AddSubMenuItem("Whisper (streaming)", "")
		mWhisperRealtime := mBackendMenu.AddSubMenuItem("Whisper (realtime)", "")
		switch backend {
		case "deepgram":
			mDeepgram.Check()
		case "elevenlabs":
			mElevenLabs.Check()
		case "elevenlabs_batch":
			mElevenLabsBatch.Check()
		case "whisper_stream":
			mWhisperStream.Check()
		case "whisper_realtime":
			mWhisperRealtime.Check()
		default:
			mWhisper.Check()
		}
		uncheckAllBackends := func() {
			mDeepgram.Uncheck()
			mElevenLabs.Uncheck()
			mElevenLabsBatch.Uncheck()
			mWhisper.Uncheck()
			mWhisperStream.Uncheck()
			mWhisperRealtime.Uncheck()
		}
		mDeepgram.Click(func() {
			uncheckAllBackends()
			mDeepgram.Check()
			svc.switchBackend("deepgram")
			mInfo.SetTitle("STT-Go (Deepgram Nova-3)")
		})
		mElevenLabs.Click(func() {
			uncheckAllBackends()
			mElevenLabs.Check()
			svc.switchBackend("elevenlabs")
			mInfo.SetTitle("STT-Go (ElevenLabs Scribe)")
		})
		mElevenLabsBatch.Click(func() {
			uncheckAllBackends()
			mElevenLabsBatch.Check()
			svc.switchBackend("elevenlabs_batch")
			mInfo.SetTitle("STT-Go (ElevenLabs batch)")
		})
		mWhisper.Click(func() {
			uncheckAllBackends()
			mWhisper.Check()
			svc.switchBackend("api")
			mInfo.SetTitle("STT-Go (Whisper)")
		})
		mWhisperStream.Click(func() {
			uncheckAllBackends()
			mWhisperStream.Check()
			svc.switchBackend("whisper_stream")
			mInfo.SetTitle("STT-Go (Whisper streaming)")
		})
		mWhisperRealtime.Click(func() {
			uncheckAllBackends()
			mWhisperRealtime.Check()
			svc.switchBackend("whisper_realtime")
			mInfo.SetTitle("STT-Go (Whisper realtime)")
		})

		systray.AddSeparator()
		mRestart := systray.AddMenuItem("Restart", "Restart STT-Go")
		mRestart.Click(func() {
			log.Info("[SVC] Restart requested from tray")
			exe, _ := os.Executable()
			cancel()
			closeRealtimePool()
			// Launch a new instance before quitting
			args := []string{"-backend", svc.backend}
			proc, err := os.StartProcess(exe, append([]string{exe}, args...), &os.ProcAttr{
				Dir:   filepath.Dir(exe),
				Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
			})
			if err != nil {
				log.Error("[SVC] Failed to restart", "err", err)
			} else {
				proc.Release()
				log.Info("[SVC] New instance launched, exiting current")
			}
			systray.Quit()
		})
		mQuit := systray.AddMenuItem("Quit", "Exit STT-Go")
		mQuit.Click(func() {
			cancel()
			closeRealtimePool()
			systray.Quit()
		})

		svc.onState = func(state trayState) {
			switch state {
			case stateIdle:
				systray.SetIcon(iconIdle)
				systray.SetTooltip("STT-Go: Idle")
			case stateListening:
				systray.SetIcon(iconListen)
				systray.SetTooltip("STT-Go: Listening...")
			case stateTranscribing:
				systray.SetIcon(iconTranscribe)
				systray.SetTooltip("STT-Go: Transcribing...")
			}
		}

		svc.run(ctx)
		systray.Quit()
	}, func() {
		log.Info("[SVC] STT-Go exiting")
	})
}
