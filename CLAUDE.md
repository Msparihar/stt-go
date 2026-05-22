# CLAUDE.md — STT-Go

## What is this?

Windows desktop speech-to-text app. Hold Right Alt → record → release → transcribes and auto-types into the active window. System tray app with Direct2D waveform overlay.

## Architecture

Single Go binary, 13 source files, no CGo. Uses Win32 APIs directly (waveIn, SendInput, Direct2D).

| File | Purpose |
|------|---------|
| `main.go` | Entry point, flags, config loading, `readEnvKey`, `initVocabulary` |
| `config.go` | `Config` struct, `loadConfig`, `saveConfig`, `defaultConfig`, `runSetup` (interactive CLI wizard) |
| `service.go` | `sttService` struct, lifecycle (`newSTTService`, `switchBackend`, `run`), `postProcess` |
| `hotkey.go` | `onPress` / `onRelease` — Right Alt poll loop, debounce, start/stop recording |
| `race.go` | `raceTranscribe` — 2s streaming head-start then parallel REST fallback; `compareWithWhisper` background mismatch logging |
| `tray.go` | System tray icon, `makeICO`, `setupTray` menu (backend switch + mic selection) |
| `audio_files.go` | `saveAudioToDisk`, `saveDebugAudio`, `cleanupOldFiles` |
| `retry.go` | retry wrapper with transient/permanent error classification and budget |
| `httpclient.go` | per-backend isolated HTTP clients (separate connection pools) |
| `deepgram.go` | `dgConn` — Deepgram Nova-3 WebSocket streaming, buffering, `CloseStream` finalize, `wasDropped` flag |
| `elevenlabs.go` | `elConn` — ElevenLabs Scribe v2 realtime WebSocket streaming, same pattern as Deepgram |
| `whisper_stream.go` | OpenAI gpt-4o-mini-transcribe SSE streaming |
| `whisper_realtime.go` / `whisper_realtime_pool.go` | Whisper realtime WebSocket + worker pool |
| `recorder.go` | `recorder` — Windows waveIn audio capture, 16kHz/16-bit/mono, device enumeration |
| `overlay.go` | `waveOverlay` — Direct2D animated waveform, topmost transparent window |
| `clipboard.go` | Ctrl+Shift+V paste-path hotkey |

## Key Patterns

### Race transcription (`raceTranscribe` in service.go)
1. Streaming backend (Deepgram/ElevenLabs) gets a **2-second head start**
2. If streaming delivers a clean result within 2s → use it immediately (~300-700ms typical)
3. If streaming is slow/dropped → fire **Whisper REST + ElevenLabs REST in parallel**
4. Collect ALL results, pick the **longest** (most complete) transcript — prevents partial transcripts from dropped connections
5. If all backends fail → `saveAudioToDisk` to `failed-audio/`
6. Hard timeout: 15s — if nothing responds, audio is saved to disk

### On-demand connections
Both `dgConn` and `elConn` connect fresh per recording (on hotkey press), buffer audio until WebSocket ready, flush buffered chunks, then stream. Connection closed after each transcription.

### Config priority for API keys (`readEnvKey`)
`config.json` → env var → `~/.env.local`

### Post-processing (`postProcess` in service.go)
Case-insensitive find-replace from `config.json` `replacements` map. Applied after transcription, before typing.

### Background comparison (`compareWithWhisper` in service.go)
Every non-Whisper transcription is re-transcribed with Whisper in background. Mismatches logged to `mismatches.jsonl` for accuracy monitoring.

### Mic persistence
`mic_device` in `config.json` stores the selected mic name. On startup, matched against `listMics()` by name. Falls back to system default (`WAVE_MAPPER`) if not found. Saved automatically when user selects from tray menu.

## Config

`config.json` next to exe. Created by `--setup` or auto-generated with defaults on first run.

```json
{
  "default_backend": "deepgram",
  "language": "en",
  "mic_device": "Microphone (Brio 100)",
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
- **Failed audio:** `failed-audio/` — saved when ALL backends fail, can be manually transcribed later
- **Mismatches:** `mismatches.jsonl` — compare primary backend vs Whisper

### Log tags (filter with `grep "[TAG]"`)

| Tag | Component |
|-----|-----------|
| `[KEY]` | Hotkey press/release, hold duration |
| `[REC]` | Recorder: start, chunk progress (every 10 chunks), stop, cleanup, waveIn API results |
| `[DG]` | Deepgram: connect, send, buffer, finalize, CloseStream |
| `[EL]` | ElevenLabs: connect, stream, finalize |
| `[WH]` | Whisper API, background comparison, mismatches |
| `[RACE]` | Race logic: deadline, fallbacks fired, winner selection, longest-pick |
| `[STT]` | Final transcription result (always logged) |
| `[TYPE]` | Text typing: RightAlt wait, foreground restore, SendInput |
| `[CFG]` | Config load/save, mic selection persistence |
| `[SVC]` | Service lifecycle, backend switching, startup/shutdown |
| `[CLIP]` | Clipboard paste-path hotkey |

### Example log queries
```bash
# See all final transcription results
grep "\[STT\]" stt-go.log | tail -20

# Debug recording issues (was audio captured?)
grep "\[REC\]" stt-go.log | tail -30

# Debug Deepgram connection drops
grep "\[DG\]" stt-go.log | tail -30

# See race decisions (which backend won?)
grep "\[RACE\]" stt-go.log | tail -20

# Full pipeline for last recording (all tags)
grep "\[KEY\]\|\[REC\]\|\[DG\]\|\[RACE\]\|\[STT\]" stt-go.log | tail -40
```

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
