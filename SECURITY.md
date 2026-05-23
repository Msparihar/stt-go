# Security Policy

## Supported versions

The latest minor release on `main` is supported. Older tagged releases receive fixes only on request.

## Reporting a vulnerability

Open a private [security advisory](https://github.com/Msparihar/stt-go/security/advisories/new) on GitHub. Do not file public issues for security problems.

I aim to acknowledge within 72 hours and ship a fix or mitigation within 14 days for confirmed issues.

## What this app stores locally

- **API keys** for the configured speech-to-text providers (OpenAI / Deepgram / ElevenLabs / Groq). Today they sit in plaintext inside `%APPDATA%\stt-go\config.json` (or the repo-local `config.json` when run from source). A migration to Windows Credential Manager is on the roadmap — when it lands, that becomes the default and `config.json` becomes a fallback with a startup warning.
- **Captured audio** in `debug-audio\` and `failed-audio\` (PCM `.wav` files). These are local only; the app never uploads them anywhere except to the transcription provider you selected, and only the audio for the current recording. Old captures are auto-cleaned on startup, but you should still clear those directories if you record sensitive content.
- **Transcription logs** in `stt-go.log`, including full transcripts. Logs rotate and old ones are gzipped. Treat the log directory as you would treat the transcripts themselves.

## Threat model

- The app does not listen unless you hold the configured hotkey (Right-Alt by default).
- All transcription traffic goes over TLS to the upstream provider.
- The app does not include any telemetry, analytics, or auto-update calls.

## Out of scope

- Operating-system-level keylogging or audio hooks. STT-Go uses the standard Win32 `waveIn*` APIs; it does not bypass OS audio permissions.
- Provider-side handling of your audio. Read your transcription provider's privacy policy.
