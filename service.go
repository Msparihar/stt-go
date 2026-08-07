package main

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

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

type sttService struct {
	backend    string
	lang       string
	apiKey     string
	openaiKey  string // cached at startup; avoids hot-path file reads
	groqKey    string // cached at startup; avoids hot-path file reads
	rec        *recorder
	dgc        *dgConn
	elc        *elConn
	log        *slog.Logger
	onState    func(trayState)
	overlay    *waveOverlay
	recT0      time.Time
	targetHwnd uintptr // foreground window when recording started

	// Real-time streaming state (non-nil only while a streaming session is active)
	rtSession   *rtSession
	rtTyper     *rtTyper
	rtMu        sync.Mutex
	rtFinalText string
	rtCtxCancel context.CancelFunc
}

func newSTTService(backend, lang string, log *slog.Logger) *sttService {
	s := &sttService{backend: backend, lang: lang, rec: newRecorder(log), log: log}

	// Cache keys that are needed across multiple backends or on every recording.
	s.openaiKey = readEnvKey("OPENAI_API_KEY")
	s.groqKey = readEnvKey("GROQ_API_KEY")

	switch backend {
	case "api":
		s.apiKey = s.openaiKey
		if s.apiKey == "" {
			log.Error("[SVC] OPENAI_API_KEY not found")
		}
		log.Info("[SVC] OpenAI client ready")
	case "whisper_stream":
		s.apiKey = s.openaiKey
		if s.apiKey == "" {
			log.Error("[SVC] OPENAI_API_KEY not found")
		}
		log.Info("[SVC] Whisper streaming client ready")
	case "whisper_realtime":
		s.apiKey = s.openaiKey
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
	case "groq":
		s.apiKey = s.groqKey
		if s.apiKey == "" {
			log.Error("[SVC] GROQ_API_KEY not found")
		}
		log.Info("[SVC] Groq Whisper client ready")
	case "whisper_local":
		ensureLocalWhisperSidecar(log) // spawn sidecar if needed, then warm the model
		log.Info("[SVC] Local Whisper client ready (faster-whisper sidecar)")
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
	// Free local-Whisper VRAM when switching away from it.
	if s.backend == "whisper_local" && backend != "whisper_local" {
		unloadLocalWhisper(s.log)
	}

	s.backend = backend
	switch backend {
	case "api":
		s.apiKey = s.openaiKey
		if s.apiKey == "" {
			s.log.Error("[SVC] OPENAI_API_KEY not found")
		}
		s.log.Info("[SVC] Switched to Whisper")
	case "whisper_stream":
		s.apiKey = s.openaiKey
		if s.apiKey == "" {
			s.log.Error("[SVC] OPENAI_API_KEY not found")
		}
		s.log.Info("[SVC] Switched to Whisper Streaming")
	case "whisper_realtime":
		s.apiKey = s.openaiKey
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
	case "groq":
		s.apiKey = s.groqKey
		if s.apiKey == "" {
			s.log.Error("[SVC] GROQ_API_KEY not found")
		}
		s.log.Info("[SVC] Switched to Groq Whisper")
	case "whisper_local":
		ensureLocalWhisperSidecar(s.log) // spawn sidecar if needed, then warm the model
		s.log.Info("[SVC] Switched to Local Whisper (faster-whisper sidecar)")
	}
}

func (s *sttService) run(ctx context.Context) {
	s.log.Info("[SVC] STT Service running — hold " + hotkeyName + " to record, release to transcribe")

	pressed := false
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			s.log.Info("[SVC] STT Service stopped")
			return
		case <-tick.C:
			down := hotkeyDown()
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
