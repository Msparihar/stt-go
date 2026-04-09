# CLAUDE.md — STT-Go

## What is this?

Windows desktop speech-to-text app. Hold Right Alt → record → release → transcribes and auto-types into the active window. System tray app with Direct2D waveform overlay.

## Architecture

Single Go binary, 8 source files, no CGo. Uses Win32 APIs directly (waveIn, SendInput, Direct2D).

| File | Purpose |
|------|---------|
| `main.go` | Entry point, flags, config loading, `readEnvKey`, `initVocabulary` |
| `config.go` | `Config` struct, `loadConfig`, `saveConfig`, `defaultConfig`, `runSetup` (interactive CLI wizard) |
| `service.go` | `sttService` — hotkey loop (Right Alt polling), `onPress`/`onRelease`, fallback orchestration, post-processing, `compareWithWhisper`, tray setup, debug audio save |
| `deepgram.go` | `dgConn` — Deepgram Nova-3 WebSocket streaming, buffering, `CloseStream` finalize, `wasDropped` flag |
| `elevenlabs.go` | `elConn` — ElevenLabs Scribe v2 realtime WebSocket streaming, same pattern as Deepgram |
| `whisper.go` | `transcribeWhisper` (OpenAI REST), `transcribeElevenLabsREST` (Scribe v2 batch), `transcribeParallelFallback`, `typeText` (SendInput), `pcmToWAV` |
| `recorder.go` | `recorder` — Windows waveIn audio capture, 16kHz/16-bit/mono, device enumeration |
| `overlay.go` | `waveOverlay` — Direct2D animated waveform, topmost transparent window |
| `clipboard.go` | Ctrl+Shift+B paste-path hotkey |

## Key Patterns

### Fallback chain (in `service.go onRelease`)
1. Primary streaming backend (Deepgram or ElevenLabs realtime)
2. If empty/dropped → `transcribeParallelFallback`: Whisper + ElevenLabs REST in parallel, first wins
3. If all fail → `saveAudioToDisk` to `failed-audio/`

### On-demand connections
Both `dgConn` and `elConn` connect fresh per recording (on hotkey press), buffer audio until WebSocket ready, flush buffered chunks, then stream. Connection closed after each transcription.

### Config priority for API keys (`readEnvKey`)
`config.json` → env var → `~/.env.local`

### Post-processing (`postProcess` in service.go)
Case-insensitive find-replace from `config.json` `replacements` map. Applied after transcription, before typing.

### Background comparison (`compareWithWhisper` in service.go)
Every non-Whisper transcription is re-transcribed with Whisper in background. Mismatches logged to `mismatches.jsonl` for accuracy monitoring.

## Config

`config.json` next to exe. Created by `--setup` or auto-generated with defaults on first run.

```json
{
  "default_backend": "deepgram",
  "language": "en",
  "keyterms": ["..."],
  "replacements": {"from": "to"},
  "api_keys": {"deepgram": "", "openai": "", "elevenlabs": ""}
}
```

## Build & Run

```bash
# Build (from WSL)
powershell.exe -Command "Set-Location 'C:\Users\manis\scripts\stt-go'; go build -ldflags '-H windowsgui' -o stt-go.exe ."

# Run (launches in background)
cmd.exe /c "start /B C:\Users\manis\scripts\stt-go\stt-go.exe -backend deepgram" 2>&1 &

# Kill + restart
powershell.exe -Command "Get-Process stt-go -ErrorAction SilentlyContinue | Stop-Process -Force; Start-Sleep -Seconds 1; Start-Process 'C:\Users\manis\scripts\stt-go\stt-go.exe' -ArgumentList '-backend','deepgram' -WindowStyle Hidden"
```

## Debugging

- **Log file:** `stt-go.log` (lumberjack, 5MB rotation, 3 backups)
- **Debug audio:** Every recording saved to `debug-audio/` as WAV (auto-cleaned after 7 days)
- **Mismatches:** `mismatches.jsonl` — compare primary backend vs Whisper
- **Check logs from WSL:** `grep "STT result" /mnt/c/Users/manis/scripts/stt-go/stt-go.log | tail -20`

## Common Issues

- **Deepgram truncation:** Fixed by waiting for connection close after `CloseStream` instead of first `is_final`. Debug via `Deepgram msg` log lines.
- **ElevenLabs realtime empty:** Check for `ElevenLabs input error` in logs. The commit format is `input_audio_chunk` with `commit:true`, NOT `{"message_type":"commit"}`.
- **`previous_text` max 50 chars** for ElevenLabs realtime — longer values silently ignored.
- **No keyterms in realtime** — ElevenLabs realtime WebSocket doesn't support keyterms (batch API only). Deepgram supports keyterms in streaming.
- **Windows GUI logging:** `io.MultiWriter` fails silently when stdout is NUL. Use `resilientWriter` (always writes to logFile).

## Audio Constants
- 16kHz, mono, 16-bit PCM
- 100ms buffer chunks (3,200 bytes each)
- Hotkey: Right Alt (VK_RMENU = 0xA5)

## Git & Releases

- Repo: https://github.com/Msparihar/stt-go
- License: Elastic License 2.0 (ELv2)
- Versioning: semver via git tags
- GoReleaser config in `.goreleaser.yml`

## Rules for Contributors

1. Build tag `//go:build windows` on every `.go` file
2. No CGo — use `syscall`/`windows` package for Win32 APIs
3. Test builds: `go build -ldflags '-H windowsgui'` — must compile clean
4. Keep it simple — this is a single-purpose productivity tool
