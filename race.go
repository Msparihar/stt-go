//go:build windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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
