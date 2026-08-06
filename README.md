<div align="center">

# STT-Go

**Hold a key, talk, release — the text lands in whatever window you're in.**

<!-- TODO(manish): replace with docs/demo.gif from ScreenToGif -->
![demo](docs/demo.gif)

[![CI](https://github.com/Msparihar/stt-go/actions/workflows/ci.yml/badge.svg)](https://github.com/Msparihar/stt-go/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/Msparihar/stt-go)](https://github.com/Msparihar/stt-go/releases)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)
[![Platform: Windows | macOS](https://img.shields.io/badge/platform-Windows%20%7C%20macOS-0078D4)](#)

</div>

---

## Why STT-Go?

Most dictation tools are either locked to one vendor, slow because they wait for a full REST round-trip, or so accurate at the expense of latency that you give up halfway through. STT-Go runs a streaming backend and a REST backend at the same time and uses whichever finishes first — so you get streaming-speed latency with REST-grade accuracy as a fallback. It lives in your system tray, costs nothing beyond your own API key, and never sends audio to a server you didn't choose.

## Features

- **7 transcription backends.** OpenAI Whisper (REST), Whisper streaming (SSE), Whisper Realtime (WebSocket), Deepgram Nova-3, ElevenLabs Scribe (streaming + batch), Groq Whisper, and **Local Whisper (GPU, offline)**. Pick one or let race mode decide.
- **Push-to-talk dictation.** Hold **Right-Alt** (Windows) or **Right-Option** (macOS), speak, release. The transcript types itself into the focused window — works in your editor, browser, terminal, Slack, anywhere.
- **System tray with live backend switching.** Right-click the tray icon to swap backends, change microphone, toggle real-time streaming, or quit — no restart needed.
- **Clipboard image passthrough.** Press **Ctrl+Shift+V** and STT-Go grabs the clipboard image (or file), saves it under `~/Pictures/clipboard/clipboard_<timestamp>.png`, and types the full path into the focused window. Handy for pasting screenshots into agents that only accept file paths.
- **Keyterm hinting.** Boost recognition of technical vocabulary (product names, framework names, CLI tools) via a configurable keyterms list — used as Deepgram keyterms and as the Whisper prompt.
- **Regex-style replacements.** Map common mistranscriptions (`"11 labs"` → `"ElevenLabs"`, `"high key"` → `"Haiku"`) via a simple `from → to` dictionary.
- **Race mode.** Runs a streaming backend and a REST backend in parallel; whichever returns a clean transcript first wins. Streaming gets a 2-second head start before REST competes.
- **Local-only audio.** Audio is captured natively (Win32 `waveIn` on Windows, miniaudio on macOS; 16 kHz / 16-bit / mono), buffered in memory, and sent only to the backend you configured. Nothing is logged or uploaded anywhere else.
- **Windows and macOS.** Same binary layout on both. On Apple Silicon the local model runs on the GPU via [MLX](https://github.com/ml-explore/mlx) — ~220 ms per dictation on an M-series chip, no API key.

## Install

### Recommended: download the release

Grab `stt-go.exe` from the [Releases](https://github.com/Msparihar/stt-go/releases) page and drop it anywhere you like. No installer.

### Coming soon: winget

```powershell
winget install Msparihar.stt-go
```

*Pending submission to winget-pkgs.*

### From source

```powershell
git clone https://github.com/Msparihar/stt-go.git
cd stt-go
go build -ldflags "-H windowsgui" -o stt-go.exe .
```

Requires Go 1.21+ and Windows. The `-H windowsgui` flag suppresses the console window.

### macOS (Apple Silicon)

Download `stt-go-darwin-arm64` from [Releases](https://github.com/Msparihar/stt-go/releases), or build from source (needs Go 1.21+ and Xcode command line tools):

```bash
git clone https://github.com/Msparihar/stt-go.git
cd stt-go
go build -o stt-go .
./stt-go
```

Grant three permissions to the app you launch it from (your terminal, until you wrap it in an app bundle), all under **System Settings → Privacy & Security**:

1. **Input Monitoring** — to see the Right-Option hotkey
2. **Accessibility** — to type the transcript into the focused app
3. **Microphone** — macOS prompts on the first recording

Relaunch the terminal after granting. The hotkey is **Right-Option**; recording state shows in the menu bar (🔴 recording, ✍️ typing). The waveform overlay and Ctrl+Shift+V clipboard helper are Windows-only for now.

## Get an API key

You only need one — pick the backend you want.

| Backend | Get a key |
|---|---|
| OpenAI (Whisper / Whisper streaming / Realtime) | <https://platform.openai.com/api-keys> |
| Deepgram | <https://console.deepgram.com/> |
| ElevenLabs | <https://elevenlabs.io/app/settings/api-keys> |
| Groq | <https://console.groq.com/keys> |
| Local Whisper (GPU) | No key needed — see [Local Whisper setup](#local-whisper-gpu-offline) |

## First run

Run the interactive setup wizard — it asks which backend you want and prompts for the matching key:

```powershell
stt-go.exe --setup
```

Prefer environment variables? Set any of these before launching:

- `OPENAI_API_KEY`
- `DEEPGRAM_API_KEY`
- `ELEVENLABS_API_KEY`
- `GROQ_API_KEY`

Or drop a `~/.env.local` file with the same names — STT-Go reads keys in this order: `config.json` → process env → `~/.env.local`.

## Usage

1. Launch `stt-go.exe`. A microphone icon appears in the system tray.
2. Put your cursor wherever you want text to land — editor, browser, chat box, anywhere.
3. **Hold Right-Alt**, speak, **release**.
4. The transcript types itself into the focused window.

Right-click the tray icon to:

- Switch backend (Deepgram, ElevenLabs Scribe, ElevenLabs batch, Whisper, Whisper streaming, Whisper realtime, Groq Whisper, Whisper Local GPU)
- Pick a microphone
- Toggle real-time streaming
- Restart or quit

**Bonus:** press **Ctrl+Shift+V** at any time to dump the clipboard image to disk and type its file path into the focused window.

### How race mode works

When you release Right-Alt, STT-Go fires the streaming backend and the REST backend in parallel:

```
release Right-Alt
   │
   ├── streaming backend (Deepgram / ElevenLabs WS / Whisper SSE)
   │       ↑ 2-second head start. Clean result within 2s → wins.
   │
   └── REST backend (Whisper / ElevenLabs batch)
           ↑ if streaming stalls, first REST result wins.

→ winner is typed into the focused window
```

The 2-second `raceStreamingDeadline` is what makes the common case feel instant while the slow case still ships an answer.

## Local Whisper (GPU, offline)

STT-Go ships a Python sidecar (`sidecar/server.py`) that runs Whisper locally on your GPU — [faster-whisper](https://github.com/SYSTRAN/faster-whisper) on NVIDIA/CUDA, [mlx-whisper](https://github.com/ml-explore/mlx-examples) on Apple Silicon (picked automatically). No API key, no data leaving the machine: ~0.9 s per dictation on a 4 GB consumer GPU, ~220 ms on an Apple M-series chip.

**Requirements**

- NVIDIA GPU with CUDA 12 runtime, or any Apple Silicon Mac
- ~2.4 GB VRAM / unified memory while the model is loaded (`large-v3-turbo`; idle process uses ~240 MB)
- Python 3.10+

**Setup**

```powershell
# Windows
cd sidecar
python -m venv .venv
.venv\Scripts\pip install -r requirements.txt
```

```bash
# macOS
cd sidecar
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt
```

The first time STT-Go activates the `whisper_local` backend it downloads `large-v3-turbo` (~1.6 GB) from Hugging Face (into `sidecar/models/` on Windows, the shared `~/.cache/huggingface` on macOS). Subsequent loads are instant.

**How it works**

When you select "Whisper Local (GPU)" from the tray, STT-Go:

1. Checks whether the sidecar is already running on `127.0.0.1:5111`.
2. If not, spawns `sidecar/.venv/Scripts/pythonw.exe sidecar/server.py` in the background (hidden window, log at `sidecar/server.out.log`).
3. Sends `POST /load` to pull the model into VRAM.
4. On every dictation, posts the WAV to `POST /transcribe` and receives JSON `{text}`.
5. When you switch away, sends `POST /unload` to free VRAM.

**Configuration** (optional overrides in `config.json`)

| Field | Default | Purpose |
|---|---|---|
| `local_whisper_url` | `http://127.0.0.1:5111/transcribe` | Sidecar endpoint |
| `local_whisper_python` | `<exeDir>/sidecar/.venv/Scripts/pythonw.exe` (Windows) / `.venv/bin/python3` (macOS) | Python interpreter path |
| `local_whisper_script` | `<exeDir>/sidecar/server.py` | Sidecar script path |

Override `WHISPER_MODEL` (default `large-v3-turbo`) or `WHISPER_PORT` (default `5111`) via environment variables before the sidecar starts.

**Tray indicators**

- Tray icon turns **red** when the backend is `whisper_local` but the sidecar is unreachable.
- The "Local model:" status item in the tray menu shows `connected`, `idle (not loaded)`, or `offline`.
- Use **"Restart local model"** from the tray to kill a stale sidecar and respawn it without restarting STT-Go.

## Configuration

Config lives in `config.json` next to the executable. STT-Go creates a default one on first run.

| Field | Purpose |
|---|---|
| `default_backend` | `deepgram`, `elevenlabs`, `elevenlabs_batch`, `api` (Whisper REST), `whisper_stream`, `whisper_realtime`, `groq`, or `whisper_local` |
| `language` | Language code, e.g. `"en"` |
| `mic_device` | Saved microphone name. Empty = system default |
| `keyterms` | Vocabulary hints fed to Deepgram + Whisper |
| `replacements` | `from → to` post-processing map |
| `streaming_mode` | Enable real-time streaming when the backend supports it |
| `realtime_model` | OpenAI Realtime model id (default `gpt-4o-mini-transcribe`) |
| `groq_model` | Groq model id (default `whisper-large-v3`) |
| `local_whisper_url` | Sidecar endpoint (default `http://127.0.0.1:5111/transcribe`) |
| `local_whisper_python` | Python interpreter for the sidecar (default: `<exeDir>/sidecar/.venv/Scripts/pythonw.exe`) |
| `local_whisper_script` | Sidecar script path (default: `<exeDir>/sidecar/server.py`) |
| `api_keys` | `deepgram`, `openai`, `elevenlabs`, `groq` |

See [`config.example.json`](config.example.json) for a complete reference.

### CLI flags

| Flag | Effect |
|---|---|
| `--setup` | Run the interactive setup wizard |
| `--backend <name>` | Override `default_backend` for this run |
| `--language <code>` | Override `language` for this run |
| `--no-tray` | Run without the system tray icon |

## Troubleshooting

**SmartScreen "Unknown Publisher" warning.** Until signed builds ship via the SignPath OSS Foundation, click **More info → Run anyway**. Code-signing status is tracked in [`docs/OSS-READINESS.md`](docs/OSS-READINESS.md).

**Microphone not detected.** Open the tray menu → Microphone and pick the device, or set `mic_device` in `config.json` to the exact name shown in Windows Sound Settings. An empty string falls back to the system default.

**`auth_error` in `stt-go.log`.** Your API key is missing or invalid. Re-run `stt-go.exe --setup`, or check `config.json` / `~/.env.local`.

**Nothing happens on Right-Alt.** Another app may have grabbed the hotkey. Check `stt-go.log` for `[KEY]` entries — if you don't see press/release events, kill any other hotkey listeners and restart.

**Logs.** `stt-go.log` next to the executable. Rotates at 5 MB, keeps 3 backups, gzipped. Useful tags: `[KEY]`, `[REC]`, `[DG]`, `[RACE]`, `[STT]`, `[WHISPER]`, `[EL-REST]`, `[CONFIG]`.

## Roadmap

- Signed Windows builds via SignPath OSS Foundation
- winget package
- Per-app backend overrides (e.g. always use Whisper inside VS Code)
- Custom hotkey binding via config

**Windows-only by design.** macOS / Linux are not on the roadmap — audio capture, hotkeys, and the overlay all use Win32 primitives (`waveIn`, Direct2D, `user32`) that would require a full rewrite per platform.

## Contributing

Bug reports and PRs welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md). Security issues: [SECURITY.md](SECURITY.md).

## License

MIT — see [LICENSE](LICENSE).

## Acknowledgements

Transcription APIs:

- [OpenAI](https://platform.openai.com/) — Whisper, gpt-4o-mini-transcribe, Realtime
- [Deepgram](https://deepgram.com/) — Nova-3
- [ElevenLabs](https://elevenlabs.io/) — Scribe
- [Groq](https://groq.com/) — hosted Whisper

Go libraries:

- [`gorilla/websocket`](https://github.com/gorilla/websocket) — WebSocket client for streaming backends
- [`energye/systray`](https://github.com/energye/systray) — Windows system tray
- [`natefinch/lumberjack`](https://github.com/natefinch/lumberjack) — log file rotation
