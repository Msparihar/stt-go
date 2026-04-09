//go:build windows

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
	"gopkg.in/natefinch/lumberjack.v2"
)

// ── resilientWriter wraps two writers, ensuring the second always gets
// the data even if the first errors (e.g. stdout in -H windowsgui mode).
type resilientWriter struct {
	primary   io.Writer // stdout — may be NUL
	secondary io.Writer // log file — must always receive data
}

func (w *resilientWriter) Write(p []byte) (int, error) {
	w.primary.Write(p) // ignore error — stdout may be broken in windowsgui
	return w.secondary.Write(p)
}

// ── Audio constants ────────────────────────────────────────────────

const (
	sampleRate     = 16000
	audioCh        = 1
	bitsPerSample  = 16
	blockAlign     = audioCh * bitsPerSample / 8
	avgBytesPerSec = sampleRate * blockAlign

	bufDurationMs = 100
	bufSize       = sampleRate * blockAlign * bufDurationMs / 1000
	numBufs       = 4

	vkRMenu    = 0xA5
	waveMapper = 0xFFFFFFFF
	wavFmtPCM  = 1
	cbEvent    = 0x00050000
	whdrDone   = 0x00000001
	inputKbd   = 1
	kfUnicode  = 0x0004
	kfKeyup    = 0x0002
)

// ── App-level config (loaded at startup) ──────────────────────────

// appConfig is the package-level config loaded from config.json.
var appConfig *Config

// ── Shared tech vocabulary (used by Whisper prompt + Deepgram keyterms) ───────

// techTerms is populated from appConfig.Keyterms after config loads.
var techTerms []string

// whisperPrompt is the full vocabulary hint for Whisper (broader than Deepgram keyterms).
var whisperPrompt string

func initVocabulary() {
	techTerms = appConfig.Keyterms
	whisperPrompt = strings.Join([]string{
		strings.Join(techTerms, ", "),
		"SSH, API, REST, JSON, YAML, TOML, npm, yarn, pip, .env, package.json",
		"COM, DLL, cron, vim, PR, CI/CD, linting, Notion, Slack, Discord",
		"setx, hooks, worktree",
	}, ", ")
}

// ── Windows DLL procs ──────────────────────────────────────────────

var (
	user32           = windows.NewLazyDLL("user32.dll")
	winmm            = windows.NewLazyDLL("winmm.dll")
	d2d1             = windows.NewLazyDLL("d2d1.dll")
	gdi32            = windows.NewLazyDLL("gdi32.dll")

	pGetAsyncKey         = user32.NewProc("GetAsyncKeyState")
	pSendInput           = user32.NewProc("SendInput")
	pGetForegroundWindow = user32.NewProc("GetForegroundWindow")
	pSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	pRegisterClassExW    = user32.NewProc("RegisterClassExW")
	pCreateWindowExW     = user32.NewProc("CreateWindowExW")
	pShowWindow          = user32.NewProc("ShowWindow")
	pDefWindowProcW      = user32.NewProc("DefWindowProcW")
	pGetMessageW         = user32.NewProc("GetMessageW")
	pTranslateMessage    = user32.NewProc("TranslateMessage")
	pDispatchMessageW    = user32.NewProc("DispatchMessageW")
	pPostMessageW        = user32.NewProc("PostMessageW")
	pSetLayeredWndAttr   = user32.NewProc("SetLayeredWindowAttributes")
	pSystemParamInfo     = user32.NewProc("SystemParametersInfoW")
	pInvalidateRect      = user32.NewProc("InvalidateRect")
	pBeginPaint          = user32.NewProc("BeginPaint")
	pEndPaint            = user32.NewProc("EndPaint")
	pSetWindowRgn        = user32.NewProc("SetWindowRgn")

	pWaveInOpen        = winmm.NewProc("waveInOpen")
	pWaveInClose       = winmm.NewProc("waveInClose")
	pWaveInPrepHdr     = winmm.NewProc("waveInPrepareHeader")
	pWaveInUnprepHdr   = winmm.NewProc("waveInUnprepareHeader")
	pWaveInAddBuf      = winmm.NewProc("waveInAddBuffer")
	pWaveInStart       = winmm.NewProc("waveInStart")
	pWaveInStop        = winmm.NewProc("waveInStop")
	pWaveInReset       = winmm.NewProc("waveInReset")
	pWaveInGetNumDevs  = winmm.NewProc("waveInGetNumDevs")
	pWaveInGetDevCapsW = winmm.NewProc("waveInGetDevCapsW")

	pMoveWindow         = user32.NewProc("MoveWindow")
	pCreateRoundRectRgn = gdi32.NewProc("CreateRoundRectRgn")
	pDeleteObject       = gdi32.NewProc("DeleteObject")
	pD2D1CreateFactory  = d2d1.NewProc("D2D1CreateFactory")
)

// ── Env reader ─────────────────────────────────────────────────────

// readEnvKey looks up an API key: config.json first, then env var, then ~/.env.local.
func readEnvKey(name string) string {
	// Check config API keys first
	if appConfig != nil {
		switch name {
		case "DEEPGRAM_API_KEY":
			if appConfig.APIKeys.Deepgram != "" {
				return appConfig.APIKeys.Deepgram
			}
		case "OPENAI_API_KEY":
			if appConfig.APIKeys.OpenAI != "" {
				return appConfig.APIKeys.OpenAI
			}
		case "ELEVENLABS_API_KEY":
			if appConfig.APIKeys.ElevenLabs != "" {
				return appConfig.APIKeys.ElevenLabs
			}
		}
	}

	// Fall back to environment variable
	if v := os.Getenv(name); v != "" {
		return v
	}

	// Fall back to ~/.env.local
	home, _ := os.UserHomeDir()
	data, err := os.ReadFile(filepath.Join(home, ".env.local"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, name+"=") {
			return strings.TrimSpace(strings.SplitN(line, "=", 2)[1])
		}
	}
	return ""
}

// ── Main ───────────────────────────────────────────────────────────

func main() {
	setup := flag.Bool("setup", false, "Run interactive setup wizard")
	backendFlag := flag.String("backend", "", "Transcription backend: api, deepgram, elevenlabs (overrides config)")
	lang := flag.String("language", "", "Language code (overrides config)")
	noTray := flag.Bool("no-tray", false, "Disable system tray icon")
	flag.Parse()

	exe, _ := os.Executable()
	logFile := &lumberjack.Logger{
		Filename:   filepath.Join(filepath.Dir(exe), "stt-go.log"),
		MaxSize:    5, // MB — rotates when exceeded
		MaxBackups: 3, // keep 3 old log files
		MaxAge:     90, // days — delete older backups
		Compress:   true, // gzip rotated files
	}

	// Verify log file is writable — lumberjack silently swallows errors.
	// Write a non-empty payload to force lumberjack to actually open the file.
	if _, err := logFile.Write([]byte("--- stt-go log start ---\n")); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: log file not writable: %v\n", err)
	}

	defer logFile.Close()

	handler := slog.NewTextHandler(&resilientWriter{primary: os.Stdout, secondary: logFile}, &slog.HandlerOptions{Level: slog.LevelInfo})
	log := slog.New(handler)

	// Load config (must happen before readEnvKey or vocabulary init)
	appConfig = loadConfig(log)
	initVocabulary()

	// Handle --setup flag
	if *setup {
		runSetup(log)
		return
	}

	// Resolve backend and language: flag overrides config
	backend := appConfig.DefaultBackend
	if *backendFlag != "" {
		backend = *backendFlag
	}
	language := appConfig.Language
	if *lang != "" {
		language = *lang
	}

	// Load replacements from config into postProcessReplacements
	postProcessReplacements = loadReplacements(appConfig.Replacements)

	svc := newSTTService(backend, language, log)
	svc.overlay = newWaveOverlay(log)

	// Clean up files older than 7 days
	cleanupOldFiles(log)

	if *noTray {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()
		go runClipboardHotkey(ctx, log)
		svc.run(ctx)
		return
	}

	setupTray(svc, backend, log)
}
