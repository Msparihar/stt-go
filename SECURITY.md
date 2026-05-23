# Security Policy

## Supported versions

The latest minor release on `main` is supported. Older tagged releases receive fixes only on request.

## Reporting a vulnerability

Open a private [security advisory](https://github.com/Msparihar/stt-go/security/advisories/new) on GitHub. Do not file public issues for security problems.

I aim to acknowledge within 72 hours and ship a fix or mitigation within 14 days for confirmed issues.

## What this app stores locally

- **API keys** for the configured speech-to-text providers (OpenAI / Deepgram / ElevenLabs / Groq). They are stored in plaintext inside `config.json` (next to `stt-go.exe`) or in `~/.env.local` — whichever you choose at setup. Bring your own credentials; treat that file the same way you'd treat any other dotenv. The app never transmits keys anywhere except to the provider you configured them for.
- **Captured audio** in `debug-audio\` and `failed-audio\` (PCM `.wav` files). These are local only; the app never uploads them anywhere except to the transcription provider you selected, and only the audio for the current recording. Old captures are auto-cleaned on startup, but you should still clear those directories if you record sensitive content.
- **Transcription logs** in `stt-go.log`, including full transcripts. Logs rotate and old ones are gzipped. Treat the log directory as you would treat the transcripts themselves.

## Threat model

- The app does not listen unless you hold the configured hotkey (Right-Alt by default).
- All transcription traffic goes over TLS to the upstream provider.
- The app does not include any telemetry, analytics, or auto-update calls.

## Out of scope

- Operating-system-level keylogging or audio hooks. STT-Go uses the standard Win32 `waveIn*` APIs; it does not bypass OS audio permissions.
- Provider-side handling of your audio. Read your transcription provider's privacy policy.
