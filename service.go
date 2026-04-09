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
			log.Error("OPENAI_API_KEY not found")
		}
		log.Info("OpenAI client ready")
	case "deepgram":
		s.apiKey = readEnvKey("DEEPGRAM_API_KEY")
		if s.apiKey == "" {
			log.Error("DEEPGRAM_API_KEY not found")
		}
		s.dgc = newDGConn(s.apiKey, lang, log)
		log.Info("Deepgram client ready (on-demand connection)")
	case "elevenlabs":
		s.apiKey = readEnvKey("ELEVENLABS_API_KEY")
		if s.apiKey == "" {
			log.Error("ELEVENLABS_API_KEY not found")
		}
		s.elc = newELConn(s.apiKey, lang, log)
		log.Info("ElevenLabs client ready (on-demand connection)")
	}
	log.Info("STT Service initialized", "backend", backend, "language", lang)
	return s
}

func (s *sttService) switchBackend(backend string) {
	if backend == s.backend {
		return
	}
	s.log.Info("Switching backend", "from", s.backend, "to", backend)

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
			s.log.Error("OPENAI_API_KEY not found")
		}
		s.log.Info("Switched to Whisper")
	case "deepgram":
		s.apiKey = readEnvKey("DEEPGRAM_API_KEY")
		if s.apiKey == "" {
			s.log.Error("DEEPGRAM_API_KEY not found")
		}
		s.dgc = newDGConn(s.apiKey, s.lang, s.log)
		s.log.Info("Switched to Deepgram (on-demand connection)")
	case "elevenlabs":
		s.apiKey = readEnvKey("ELEVENLABS_API_KEY")
		if s.apiKey == "" {
			s.log.Error("ELEVENLABS_API_KEY not found")
		}
		s.elc = newELConn(s.apiKey, s.lang, s.log)
		s.log.Info("Switched to ElevenLabs (on-demand connection)")
	}
}

func (s *sttService) onPress() {
	// Save the foreground window before recording starts so we can
	// restore it before typing (overlay or other windows may steal focus)
	hwnd, _, _ := pGetForegroundWindow.Call()
	s.targetHwnd = hwnd
	s.log.Info("onPress: captured foreground window", "hwnd", fmt.Sprintf("0x%X", hwnd))

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
	default: // "api" (Whisper)
		s.rec.onChunk = func(data []byte) {
			s.rec.allData = append(s.rec.allData, data...)
			if s.overlay != nil {
				s.overlay.pushAudio(data)
			}
		}
	}

	if err := s.rec.start(); err != nil {
		s.log.Error("Recording failed", "err", err)
		return
	}
}

func (s *sttService) onRelease() {
	if s.overlay != nil {
		s.overlay.showTranscribing()
	}
	targetHwnd := s.targetHwnd
	go func() {
		defer func() {
			if p := recover(); p != nil {
				s.log.Error("onRelease: panic recovered", "panic", fmt.Sprintf("%v", p))
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
			s.log.Info("Too short, ignoring", "backend", s.backend, "duration", fmt.Sprintf("%.2fs", duration))
			return
		}

		// Save every recording to debug-audio/ for diagnosis
		debugFile := saveDebugAudio(s.rec.allData, s.log)

		transcribeStart := time.Now()
		usedBackend := s.backend
		var text string
		var transcribeErr error
		switch s.backend {
		case "api":
			text, transcribeErr = transcribeWhisper(s.rec.allData, s.apiKey, s.lang, s.log)
		case "deepgram":
			text = s.dgc.finalize(s.recT0)
		case "elevenlabs":
			text = s.elc.finalize(s.recT0)
		}
		transcribeElapsed := time.Since(transcribeStart)
		sessionElapsed := time.Since(s.recT0)

		// Detect if streaming backend connection dropped (partial/no transcript)
		streamingDropped := false
		if s.backend == "deepgram" && s.dgc != nil {
			streamingDropped = s.dgc.wasDropped()
		}
		// For elevenlabs streaming, check if text is suspiciously short vs recording duration
		// (ElevenLabs doesn't have a dropped flag, but same pattern applies)
		if s.backend == "elevenlabs" && text == "" {
			streamingDropped = true
		}

		// Fallback: streaming returned empty OR connection dropped mid-recording
		needsFallback := (text == "" || streamingDropped) && s.backend != "api" && len(s.rec.allData) > 0 && duration >= 0.5
		if needsFallback {
			if text != "" {
				s.log.Warn("Streaming backend returned partial transcript (connection dropped), falling back",
					"backend", s.backend,
					"partial_text", text,
					"duration", fmt.Sprintf("%.1fs", duration),
				)
			} else {
				s.log.Warn("Streaming backend returned empty, falling back",
					"backend", s.backend,
					"duration", fmt.Sprintf("%.1fs", duration),
				)
			}

			whisperKey := readEnvKey("OPENAI_API_KEY")
			elevenLabsKey := readEnvKey("ELEVENLABS_API_KEY")

			fallbackStart := time.Now()
			fallbackText, fallbackBackend, fallbackErr := transcribeParallelFallback(
				s.rec.allData, whisperKey, elevenLabsKey, s.lang, s.log,
			)
			transcribeElapsed += time.Since(fallbackStart)
			sessionElapsed = time.Since(s.recT0)

			if fallbackErr == nil && fallbackText != "" {
				text = fallbackText
				usedBackend = s.backend + "+" + fallbackBackend
				transcribeErr = nil
			} else {
				// ALL backends failed — save audio to disk so voice is never lost
				transcribeErr = fallbackErr
				savedPath := saveAudioToDisk(s.rec.allData, s.log)
				if savedPath != "" {
					s.log.Error("All transcription backends failed — audio saved to disk",
						"path", savedPath,
						"duration", fmt.Sprintf("%.1fs", duration),
					)
				}
			}
		}

		if transcribeErr != nil {
			s.log.Error("STT result",
				"backend", usedBackend,
				"language", s.lang,
				"duration", fmt.Sprintf("%.1fs", duration),
				"transcribe_time", transcribeElapsed.Round(time.Millisecond),
				"session", sessionElapsed.Round(time.Millisecond),
				"error", transcribeErr,
			)
			return
		}

		// Unified log line for every transcription — grep "STT result" to analyze
		s.log.Info("STT result",
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
		if text != "" && usedBackend != "api" && len(s.rec.allData) > 0 {
			pcmCopy := make([]byte, len(s.rec.allData))
			copy(pcmCopy, s.rec.allData)
			primaryText := text
			primaryBackend := usedBackend
			audioFile := debugFile
			go s.compareWithWhisper(pcmCopy, primaryText, primaryBackend, audioFile, duration)
		}
	}()
}

func (s *sttService) run(ctx context.Context) {
	s.log.Info("STT Service running (Go) — hold Right Alt to record, release to transcribe")

	pressed := false
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			s.log.Info("STT Service stopped")
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
			s.log.Error("compareWithWhisper: panic", "panic", fmt.Sprintf("%v", p))
		}
	}()

	whisperKey := readEnvKey("OPENAI_API_KEY")
	if whisperKey == "" {
		return
	}

	whisperText, err := transcribeWhisper(pcm, whisperKey, s.lang, s.log)
	if err != nil {
		s.log.Warn("Background Whisper comparison failed", "err", err)
		return
	}

	// Normalize for comparison: lowercase, trim
	normPrimary := strings.ToLower(strings.TrimSpace(primaryText))
	normWhisper := strings.ToLower(strings.TrimSpace(whisperText))

	// Remove trailing punctuation for comparison
	normPrimary = strings.TrimRight(normPrimary, ".!?,;:")
	normWhisper = strings.TrimRight(normWhisper, ".!?,;:")

	if normPrimary == normWhisper {
		s.log.Info("Background Whisper match", "audio_file", audioFile)
		return
	}

	s.log.Warn("Transcription mismatch detected",
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
		log.Error("Failed to marshal mismatch", "err", err)
		return
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Error("Failed to open mismatches.jsonl", "err", err)
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
				os.Remove(filepath.Join(debugDir, e.Name()))
				removed++
			}
		}
		if removed > 0 {
			log.Info("Cleaned up old debug audio files", "removed", removed)
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
				os.Remove(filepath.Join(failedDir, e.Name()))
				removed++
			}
		}
		if removed > 0 {
			log.Info("Cleaned up old failed audio files", "removed", removed)
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
		log.Info("Cleaned up old mismatch entries", "removed", removed, "kept", len(kept))
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
		log.Error("Failed to save audio to disk", "err", err)
		return ""
	}
	log.Info("Audio saved to disk for later retry", "path", path, "size", len(wav))
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
		log.Error("Failed to save debug audio", "err", err)
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
		log.Warn("Could not load icon.ico, using fallback", "err", err)
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
			"deepgram":   "Deepgram Nova-3",
			"api":        "Whisper",
			"elevenlabs": "ElevenLabs Scribe",
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
			item.Click(func() {
				// Uncheck all, check selected
				for _, mi := range micItems {
					mi.Uncheck()
				}
				item.Check()
				svc.rec.setDeviceID(micID)
				log.Info("Switched microphone", "device", micID, "name", mic.Name)
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
		mElevenLabs := mBackendMenu.AddSubMenuItem("ElevenLabs Scribe", "")
		mWhisper := mBackendMenu.AddSubMenuItem("Whisper (OpenAI)", "")
		switch backend {
		case "deepgram":
			mDeepgram.Check()
		case "elevenlabs":
			mElevenLabs.Check()
		default:
			mWhisper.Check()
		}
		uncheckAllBackends := func() {
			mDeepgram.Uncheck()
			mElevenLabs.Uncheck()
			mWhisper.Uncheck()
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
		mWhisper.Click(func() {
			uncheckAllBackends()
			mWhisper.Check()
			svc.switchBackend("api")
			mInfo.SetTitle("STT-Go (Whisper)")
		})

		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Quit", "Exit STT-Go")
		mQuit.Click(func() {
			cancel()
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
		log.Info("STT-Go exiting")
	})
}
