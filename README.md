# STT-Go

Hold Right Alt to record. Release to transcribe and type the result into the active window.

A Windows speech-to-text desktop app that lives in the system tray. Uses streaming backends for low latency, with automatic fallback and audio preservation when things go wrong.

## Features

- **Multiple STT backends** — Deepgram Nova-3 (streaming), ElevenLabs Scribe v2 (streaming), OpenAI Whisper (REST)
- **Automatic fallback** — if the streaming backend fails, fires Whisper and ElevenLabs REST in parallel; first success wins
- **Audio never lost** — if all backends fail, raw audio is saved to `failed-audio/` as WAV for manual recovery
- **Customizable vocabulary** — keyterms to improve recognition of domain-specific words
- **Text replacements** — post-processing corrections for commonly misheard terms
- **System tray** — switch backend or microphone without restarting
- **Waveform overlay** — Direct2D animated overlay while recording
- **Accuracy monitoring** — optional background Whisper comparison; mismatches logged to `mismatches.jsonl`
- **Auto-cleanup** — debug files older than 7 days are removed automatically
- **Single binary** — no runtime dependencies

## Requirements

- Windows 10 or 11
- At least one API key: Deepgram, ElevenLabs, or OpenAI
- Go 1.21+ (only needed to build from source)

## Setup

**Option A: Download binary**

Download `stt-go.exe` from [Releases](../../releases).

**Option B: Build from source**

```
go build -ldflags "-H windowsgui" -o stt-go.exe .
```

**First run**

```
stt-go.exe --setup
```

This runs an interactive wizard to configure your API keys and preferred backend. Afterwards, just run:

```
stt-go.exe
```

The app appears in the system tray and is ready to use.

## Usage

| Action | Result |
|--------|--------|
| Hold **Right Alt** | Start recording (waveform overlay appears) |
| Release **Right Alt** | Stop recording, transcribe, type result |
| **Ctrl+Shift+B** | Paste file path from clipboard into active window |
| Right-click tray icon | Switch backend, switch microphone, quit |

## Configuration

Config file: `config.json` (next to the exe, created by `--setup`)

```json
{
  "default_backend": "deepgram",
  "language": "en",
  "keyterms": ["TypeScript", "React", "Kubernetes"],
  "replacements": {
    "high key": "Haiku",
    "code rabbit": "CodeRabbit"
  },
  "api_keys": {
    "deepgram": "your-key",
    "openai": "your-key",
    "elevenlabs": "your-key"
  }
}
```

**keyterms** — words the STT backend should weight more heavily. Useful for technical jargon, product names, or names that get mangled by default models.

**replacements** — exact-match post-processing substitutions applied after transcription. Keys are lowercased before matching.

## CLI Flags

| Flag | Description |
|------|-------------|
| `--backend <name>` | Override default backend: `deepgram`, `elevenlabs`, `api` |
| `--language <code>` | Language code (default: `en`) |
| `--setup` | Run interactive setup wizard |
| `--no-tray` | Run without system tray (foreground mode) |

## Fallback Chain

```
Recording -> Primary Backend (streaming)
                 |
                 | fail or empty result
                 v
         Whisper + ElevenLabs REST (parallel, first success wins)
                 |
                 | both fail
                 v
         Save audio to failed-audio/*.wav
```

The failed audio files can be manually submitted to any STT API or transcribed later.

## Architecture

| File | Purpose |
|------|---------|
| `main.go` | Entry point, CLI flag parsing, config loading |
| `config.go` | Config file management and setup wizard |
| `service.go` | STT orchestration, hotkey loop, fallback logic |
| `deepgram.go` | Deepgram Nova-3 WebSocket streaming |
| `elevenlabs.go` | ElevenLabs Scribe v2 WebSocket streaming |
| `whisper.go` | OpenAI Whisper REST API, ElevenLabs REST fallback, text typing |
| `recorder.go` | Windows waveIn audio capture |
| `overlay.go` | Direct2D waveform overlay |
| `clipboard.go` | Clipboard paste-path hotkey |

Uses Win32 APIs directly (waveIn, SendInput, Direct2D) — no CGo, no external GUI framework.

## License

Elastic License 2.0 (ELv2) — free to use, modify, and distribute. You may not provide it as a managed service or remove license/notice files. See [LICENSE](LICENSE) for details.
