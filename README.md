<div align="center">

# STT-Go

**The open-source Wispr Flow alternative — hold a key, speak, get text in any app.**

[![CI](https://github.com/Msparihar/stt-go/actions/workflows/ci.yml/badge.svg)](https://github.com/Msparihar/stt-go/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/Msparihar/stt-go)](https://github.com/Msparihar/stt-go/releases)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)
[![Platform: Windows | macOS](https://img.shields.io/badge/platform-Windows%20%7C%20macOS-0078D4)](#)

</div>

---

STT-Go is push-to-talk dictation for Windows and macOS. Hold a key, speak, release — the transcript types itself into the focused window: editor, browser, terminal, chat. It is a single Go binary that lives in your system tray, with nine speech engines you can switch at runtime, including a fully offline local Whisper model.

## vs Wispr Flow

| | STT-Go | Wispr Flow |
|---|---|---|
| Price | Free, MIT-licensed, open source | Subscription |
| Offline | Yes — local Whisper `large-v3-turbo` (via MLX on Apple Silicon, ~220 ms per utterance; CUDA on NVIDIA) | No — cloud only |
| Cloud engines | Bring your own key: Deepgram Nova-3, ElevenLabs Scribe, OpenAI gpt-transcribe, Groq Whisper | Their models |
| Push-to-talk key | Any modifier, set in `config.json`, no rebuild (macOS; Windows is Right-Alt) | Fixed choices |
| Your audio | Stays on your disk; goes only to the engine you picked | Their servers |
| AI auto-editing / tone polish | No — you get what you said | Yes |

If you want an assistant that rewrites your speech, Wispr Flow does more. If you want fast, private dictation you control and can read the source of, this is it.

## Features

- **Hold-to-talk, plus tap-toggle.** Hold the hotkey to dictate, or tap it to start and tap again to stop (macOS, on by default, `tap_toggle` in config).
- **Nine backends, switchable from the tray.** Deepgram Nova-3, ElevenLabs Scribe (streaming + batch), OpenAI gpt-transcribe (REST, SSE streaming, realtime, live), Groq Whisper, and local Whisper on your GPU. The menu bar shows which one is active.
- **Race mode — no take is lost.** A streaming backend gets a 2-second head start; if it stalls, REST backends fire in parallel and the best transcript wins. If every engine fails, the audio is saved to `failed-audio/` so you can transcribe it later.
- **Keyterm biasing + replacements.** Feed your jargon (product names, CLI tools) to the engine, and map common mishears (`"11 labs"` → `"ElevenLabs"`) with a `from → to` dictionary.
- **Hallucination guard.** Degenerate repetition-wall transcripts from local Whisper are never typed; the audio is retried through another engine instead.
- **Debug audio.** Every recording lands in `debug-audio/` as WAV (auto-cleaned after 7 days), so you can always check what the mic heard.

## Install

### macOS (Apple Silicon)

Build from source — needs Go 1.21+ and Xcode command line tools:

```bash
git clone https://github.com/Msparihar/stt-go.git
cd stt-go
go build -o stt-go .
./stt-go
```

Grant two permissions to the app you launch it from, under **System Settings → Privacy & Security**: **Input Monitoring** (to see the hotkey) and **Accessibility** (to type the transcript). macOS asks for the microphone on first recording. Relaunch the terminal after granting.

Default hotkey is **Ctrl** — change it with `hotkey` in the config (see below).

### Windows

Download `stt-go_<version>_windows_amd64.zip` from [Releases](https://github.com/Msparihar/stt-go/releases) and unzip anywhere. No installer. Scoop and winget manifests live in [`packaging/`](packaging/); the winget-pkgs submission is pending.

Or build from source:

```powershell
git clone https://github.com/Msparihar/stt-go.git
cd stt-go
go build -ldflags "-H windowsgui" -o stt-go.exe .
```

Hotkey is **Right-Alt**. Until signed builds ship, SmartScreen will warn — click **More info → Run anyway**.

## First run

Pick one engine and get a key ([OpenAI](https://platform.openai.com/api-keys), [Deepgram](https://console.deepgram.com/), [ElevenLabs](https://elevenlabs.io/app/settings/api-keys), [Groq](https://console.groq.com/keys)) — or use local Whisper with no key at all. Then either run the wizard:

```bash
./stt-go --setup
```

or set `OPENAI_API_KEY` / `DEEPGRAM_API_KEY` / `ELEVENLABS_API_KEY` / `GROQ_API_KEY` in the environment or `~/.env.local`. Key lookup order: `config.json` → env → `~/.env.local`.

## Local Whisper (offline, free)

A Python sidecar (`sidecar/server.py`) runs Whisper `large-v3-turbo` on your own hardware — [mlx-whisper](https://github.com/ml-explore/mlx-examples) on Apple Silicon (~220 ms per utterance), [faster-whisper](https://github.com/SYSTRAN/faster-whisper) on NVIDIA/CUDA (~0.9 s on a 4 GB GPU). No key, no audio leaves the machine.

```bash
cd sidecar
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt   # .venv\Scripts\pip on Windows
```

Select "Whisper Local (GPU)" from the tray; STT-Go spawns and manages the sidecar itself. First use downloads the model (~1.6 GB). The tray shows sidecar status and has a "Restart local model" item.

## Configuration

Config lives in `config.json` — `~/Library/Application Support/STT-Go/` on macOS, next to the exe on Windows. Created on first run; see [`config.example.json`](config.example.json).

| Field | Purpose |
|---|---|
| `hotkey` | macOS push-to-talk modifier: `ctrl` (default), `option`, `cmd`, `shift`, `fn`, or `left_`/`right_` variants. Ignored on Windows (Right-Alt). |
| `tap_toggle` | macOS: a bare tap of the hotkey toggles recording on/off (default `true`). Hold-to-talk always works. |
| `default_backend` | `deepgram`, `elevenlabs`, `elevenlabs_batch`, `api` (OpenAI REST), `whisper_stream`, `whisper_realtime`, `whisper_live`, `groq`, or `whisper_local` |
| `keyterms` | Vocabulary hints fed to Deepgram keyterms and the Whisper prompt |
| `replacements` | `from → to` fixes applied after transcription, before typing |
| `streaming_mode` | Type text live as you speak, when the backend supports it |
| `api_keys` | `deepgram`, `openai`, `elevenlabs`, `groq` |
| `mic_device` | Saved microphone name; empty = system default. Also settable from the tray. |

CLI flags: `--setup` (wizard), `--backend <name>` and `--language <code>` (one-run overrides), `--no-tray`.

## Troubleshooting

- **Nothing happens on the hotkey** — check `stt-go.log` for `[KEY]` lines; on macOS re-check Input Monitoring after any rebuild.
- **`auth_error` in the log** — key missing or wrong; re-run `--setup`.
- **Logs** — `stt-go.log` in the data dir, rotated at 5 MB. Grep tags: `[KEY]` `[REC]` `[DG]` `[RACE]` `[STT]` `[TYPE]` `[CFG]`.

Internals (architecture, per-file map, race logic) are in [CLAUDE.md](CLAUDE.md).

## Contributing

Bug reports and PRs welcome — see [CONTRIBUTING.md](CONTRIBUTING.md), [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md), [SECURITY.md](SECURITY.md).

## License

MIT — see [LICENSE](LICENSE).
