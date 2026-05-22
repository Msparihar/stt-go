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
