# Contributing to STT-Go

Thanks for your interest. STT-Go is a small Windows-only dictation tool — contributions that keep it focused are very welcome.

## Prerequisites

- Windows 10 or 11
- Go 1.25 or newer (matches `go.mod`)
- `git`, plus PowerShell or `cmd.exe` for the build

## Build

```powershell
git clone https://github.com/Msparihar/stt-go.git
cd stt-go
go build -ldflags "-H windowsgui" -o stt-go.exe .
```

The `-H windowsgui` flag suppresses the console window; drop it during debugging if you want stdout/stderr.

## Run tests

```powershell
go vet ./...
go test -race ./...
```

CI (`.github/workflows/ci.yml`) runs `go vet`, `staticcheck`, `go build`, and `go test -race`. Make sure each passes locally before opening a PR.

To install `staticcheck`:

```powershell
go install honnef.co/go/tools/cmd/staticcheck@latest
staticcheck ./...
```

## Branching and commits

- Branch off `main`. Use a short prefix: `feat/`, `fix/`, `chore/`, `docs/`, `ci/`.
- Commit messages follow `type: short summary in imperative mood`. Examples from `git log`: `feat: add Groq + realtime WS backends`, `chore: relicense to MIT for OSS distribution`.
- One logical change per commit. If a single PR touches the build pipeline, code, and docs, split them.

## Pull requests

- Open against `main`.
- Describe what changed and why. Screenshots / GIFs help for any tray, overlay, or hotkey behaviour change.
- Confirm the checklist in the PR template before requesting review.

## Scope

Bug fixes, new backends, and quality-of-life improvements (better overlay, smarter retries, sensible defaults) are all fair game.

Out of scope for now:
- macOS / Linux ports. The codebase is Win32-bound (`waveIn`, Direct2D, user32 hotkeys); supporting another OS is a rewrite, not a port.
- Telemetry, analytics, or auto-update calls.

## Code style

- Run `gofmt`. CI rejects unformatted code.
- Keep comments rare. Only write a comment when the *why* is non-obvious — a hidden constraint, an upstream quirk, a workaround for a known bug. Don't narrate what the code does.
