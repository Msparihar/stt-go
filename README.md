# STT-Go

A Windows speech-to-text app that lives in the system tray. Hold Right Alt to record; release to transcribe and auto-type the result into whatever window has focus. Uses streaming backends for low latency with automatic REST fallback and audio preservation when backends fail.

## Architecture

```
Hold Right Alt
      |
   Recorder (waveIn, 16kHz/16-bit/mono)
      |  [Deepgram WS streaming starts on keydown]
      |
Release Right Alt
      |
      +---> [Streaming backend: Deepgram / ElevenLabs WS]
      |          2s raceStreamingDeadline head-start
      |          Clean result within 2s → wins immediately
      |
      +---> [Whisper REST]   (parallel REST fallbacks)
      +---> [ElevenLabs REST]
      |
      v
  longest-wins / first-REST-wins
      |
   typeText → active window
```

The 2-second `raceStreamingDeadline` gives the streaming backend a head start. If it returns a clean transcript within that window, the REST calls are cancelled. If not, the first REST backend to succeed wins.

## Configuration

Config file: `config.json` (next to the exe). See `config.example.json` for all fields.

Required env vars (read from `config.json` → env var → `~/.env.local`):

| Variable | Used by |
|---|---|
| `DEEPGRAM_API_KEY` | Deepgram Nova-3 streaming + REST race |
| `OPENAI_API_KEY` | Whisper REST, Whisper streaming, Whisper Realtime |
| `ELEVENLABS_API_KEY` | ElevenLabs Scribe streaming, batch REST, REST race |

## Build

```powershell
powershell.exe -Command "Set-Location 'C:\Users\manis\scripts\stt-go'; go build -ldflags '-H windowsgui' -o stt-go.exe ."
```

First run — interactive setup wizard:

```
stt-go.exe --setup
```

## Hotkeys

| Hotkey | Action |
|---|---|
| Hold **Right Alt** | Start recording (waveform overlay appears) |
| Release **Right Alt** | Stop recording, transcribe, type result |
| **Ctrl+Shift+V** | Paste clipboard image path into active window |

## Logs

Log file: `stt-go.log` (5 MB rotation, 3 backups). Filter by tag:

| Tag | Component |
|---|---|
| `[KEY]` | Hotkey press/release, hold duration |
| `[REC]` | Recorder: start, chunk progress, stop |
| `[DG]` | Deepgram connect, stream, finalize |
| `[RACE]` | Race logic: deadline, fallbacks, winner |
| `[STT]` | Final transcription result (always logged) |
| `[WHISPER]` | Background Whisper comparison |
| `[EL-REST]` | ElevenLabs REST fallback |
| `[CLEANUP]` | Old file removal at startup |
| `[CONFIG]` | Config load/save, key lookups |

## License

MIT License. See [LICENSE](LICENSE).
