//go:build windows

package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

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

	// StreamingMode: open a per-session WebSocket before the recorder starts.
	// Always uses OPENAI_API_KEY regardless of the active transcription backend.
	// We still buffer audio locally as a fallback in case the WS fails mid-session.
	if appConfig.StreamingMode {
		openaiKey := readEnvKey("OPENAI_API_KEY")
		if openaiKey != "" {
			ctx, cancel := context.WithCancel(context.Background())
			s.rtCtxCancel = cancel
			s.rtFinalText = ""
			s.startRTSession(ctx, openaiKey)
		} else {
			s.log.Warn("[RT-WS] StreamingMode enabled but OPENAI_API_KEY not set, skipping")
		}
	}

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

	// If a real-time session opened successfully, tee chunks to the WebSocket.
	if s.rtSession != nil {
		prior := s.rec.onChunk
		s.rec.onChunk = func(data []byte) {
			prior(data)
			if err := s.rtSession.sendChunk(data); err != nil {
				s.log.Warn("[RT-WS] sendChunk error, dropping chunk", "err", err)
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
			// Close any open RT session and cancel its context.
			if s.rtSession != nil {
				s.rtSession.close()
				s.rtSession = nil
				s.rtTyper = nil
			}
			if s.rtCtxCancel != nil {
				s.rtCtxCancel()
				s.rtCtxCancel = nil
			}
			return
		}

		// Save every recording to debug-audio/ for diagnosis
		debugFile := saveDebugAudio(s.rec.allData, s.log)
		pcm := s.rec.allData

		transcribeStart := time.Now()
		usedBackend := s.backend
		var text string
		var transcribeErr error

		// Real-time streaming path: audio was already streamed live; text is partially
		// typed. stopRTSession commits, waits for the completed event, and applies
		// any end-of-utterance correction. We skip the normal transcription dispatch.
		// Context is cancelled AFTER stopRTSession so readPump stays alive for the
		// completed event.
		if appConfig.StreamingMode && s.rtSession != nil {
			usedBackend = "whisper_realtime_stream"
			typedBeforeStop := s.rtTyper.typed
			text = s.stopRTSession()
			s.rtTyper = nil
			if s.rtCtxCancel != nil {
				s.rtCtxCancel()
				s.rtCtxCancel = nil
			}
			// Apply keyterm replacements to the final authoritative text.
			// Live deltas are typed raw; the completed event text may still need
			// replacements (e.g. "high key" → "Haiku"). Diff against what was
			// already typed and apply only the delta so the cursor position is correct.
			if text != "" {
				processed := postProcess(text)
				if processed != text {
					// Find the longest common prefix between what was typed and processed text,
					// then backspace over the divergence and type the corrected suffix.
					tyRunes := []rune(typedBeforeStop)
					if text != typedBeforeStop {
						// stopRTSession already corrected typed→text; now correct text→processed
						tyRunes = []rune(text)
					}
					newRunes := []rune(processed)
					prefixLen := 0
					for prefixLen < len(tyRunes) && prefixLen < len(newRunes) && tyRunes[prefixLen] == newRunes[prefixLen] {
						prefixLen++
					}
					backspaces := len(tyRunes) - prefixLen
					suffix := string(newRunes[prefixLen:])
					if backspaces > 0 {
						sendBackspaces(backspaces, targetHwnd, s.log)
					}
					if suffix != "" {
						typeRunes([]rune(suffix), targetHwnd, s.log)
					}
					text = processed
				}
			}
		} else if appConfig.StreamingMode && s.rtSession == nil {
			if s.rtCtxCancel != nil {
				s.rtCtxCancel()
				s.rtCtxCancel = nil
			}
			// WebSocket failed to open — fall back to buffered whisper_stream.
			// Use cached openaiKey; the backend's s.apiKey may be a different service's key.
			s.log.Warn("[RT-WS] session unavailable, falling back to whisper_stream")
			usedBackend = "whisper_stream_fallback"
			cfg := defaultRetryConfig()
			cfg.onRetry = func() { closeIdleConns(whisperStreamHTTPClient) }
			res := withRetry(context.Background(), cfg, "whisper_stream", s.log,
				func(ctx context.Context) (string, error) {
					return transcribeWhisperStream(ctx, pcm, s.openaiKey, s.lang, s.log)
				})
			text, transcribeErr = res.text, res.err
			if transcribeErr == nil && text != "" {
				text = postProcess(text)
				typeText(text, targetHwnd, s.log)
			}
		} else {
			if s.rtCtxCancel != nil {
				s.rtCtxCancel()
				s.rtCtxCancel = nil
			}
			switch s.backend {
			case "api":
				cfg := defaultRetryConfig()
				cfg.onRetry = func() { closeIdleConns(whisperHTTPClient) }
				res := withRetry(context.Background(), cfg, "whisper", s.log,
					func(ctx context.Context) (string, error) {
						return transcribeWhisper(ctx, pcm, s.apiKey, s.lang, s.log)
					})
				text, transcribeErr = res.text, res.err
			case "whisper_stream":
				cfg := defaultRetryConfig()
				cfg.onRetry = func() { closeIdleConns(whisperStreamHTTPClient) }
				res := withRetry(context.Background(), cfg, "whisper_stream", s.log,
					func(ctx context.Context) (string, error) {
						return transcribeWhisperStream(ctx, pcm, s.apiKey, s.lang, s.log)
					})
				text, transcribeErr = res.text, res.err
			case "whisper_realtime":
				cfg := defaultRetryConfig()
				res := withRetry(context.Background(), cfg, "whisper_realtime", s.log,
					func(ctx context.Context) (string, error) {
						return transcribeWhisperRealtime(ctx, pcm, s.apiKey, s.lang, s.log)
					})
				text, transcribeErr = res.text, res.err
			case "elevenlabs_batch":
				cfg := defaultRetryConfig()
				cfg.onRetry = func() { closeIdleConns(elevenLabsRESTHTTPClient) }
				res := withRetry(context.Background(), cfg, "elevenlabs_batch", s.log,
					func(ctx context.Context) (string, error) {
						return transcribeElevenLabsREST(ctx, pcm, s.apiKey, s.lang, s.log)
					})
				text, transcribeErr = res.text, res.err
			case "groq":
				cfg := defaultRetryConfig()
				cfg.onRetry = func() { closeIdleConns(groqHTTPClient) }
				res := withRetry(context.Background(), cfg, "groq", s.log,
					func(ctx context.Context) (string, error) {
						return transcribeGroq(ctx, s.apiKey, pcm, s.log)
					})
				text, transcribeErr = res.text, res.err
			case "whisper_local":
				cfg := defaultRetryConfig()
				cfg.onRetry = func() { closeIdleConns(whisperLocalHTTPClient) }
				res := withRetry(context.Background(), cfg, "whisper_local", s.log,
					func(ctx context.Context) (string, error) {
						return transcribeWhisperLocal(ctx, pcm, s.log)
					})
				text, transcribeErr = res.text, res.err
			case "deepgram", "elevenlabs":
				text, usedBackend, transcribeErr = s.raceTranscribe(pcm, duration)
			}
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

		// In streaming mode, text was already typed live (plus any correction in stopRTSession).
		// In normal mode, type now.
		if text != "" && !appConfig.StreamingMode {
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
