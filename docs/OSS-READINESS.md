# STT-Go Open-Source Readiness Plan

Source: audit run 2026-05-23 (current score **38/100**, target **90+**). Each task below is a copy-paste brief you can hand to a subagent as-is. All implementation agents default to **Sonnet** (never Opus — pre-tool hook blocks it). Use `general-purpose` unless a more specific agent fits.

**Constraints baked in:**
- Free/OSS tooling only — no paid services (use SignPath OSS, not Azure Trusted Signing).
- Be honest: the app is Windows-only. Every `.go` file is `//go:build windows`. Don't pretend mac/Linux is on the roadmap.
- Verbatim comment-discipline line MUST appear in every implementation brief: *"No comments unless the WHY is non-obvious. No JSDoc. No step-narration comments ('1. validate args', '2. call API'). No file-header explanations of the obvious flow."*

---

## Execution order (impact / effort)

| # | Task | Blocks | Effort | Status |
|---|---|---|---|---|
| 1 | Flip LICENSE to MIT | winget/scoop, contributors | 15 min | ☐ |
| 2 | Set repo metadata (description, topics) | discoverability | 15 min | ☐ |
| 3 | Add `release.yml` GoReleaser workflow | binary distribution | 1 hr | ☐ |
| 4 | Cut `v1.1.0` tag → first auto-built release | proves pipeline | 5 min | ☐ |
| 5 | Rewrite README as landing page | adoption | 2 hr | ☐ |
| 6 | Record demo GIF (ScreenToGif) | embed in README | 1 hr | ☐ (manual) |
| 7 | Add SECURITY.md / CONTRIBUTING.md / CoC / templates | community signal | 1 hr | ☐ |
| 8 | Enable Discussions + add CHANGELOG.md | community | 15 min | ☐ |
| 9 | Drop cross-platform claims from all docs/code | honesty | 30 min | ☐ |
| 10 | Submit winget manifest | distribution | 2 hr | ☐ |
| 11 | Submit scoop manifest | distribution | 1 hr | ☐ |
| 12 | Migrate keys to Windows Credential Manager | security | 4 hr | ☐ |
| 13 | Apply for SignPath OSS code-signing | trust | 1 hr apply + wait | ☐ |
| 14 | Wire SignPath into GoReleaser | trust | 1 hr after approval | ☐ |

Tasks 1–9 push the score past 70. Add 10–11 for ~80. Add 12–14 for 90+.

---

## Task 1 — License flip to MIT

**Subagent: `general-purpose`. Effort: 15 min. Blocks: 10, 11.**

```text
Replace /mnt/c/Users/manis/scripts/stt-go/LICENSE (currently Elastic
License 2.0, source-available, NOT OSI-approved) with the MIT License.

Steps:
1. Read existing LICENSE to confirm it's ELv2.
2. Overwrite with the MIT License template. Copyright line:
   "Copyright (c) 2025 Manish Singh Parihar"
3. Update README.md license badge / footer line to MIT.
4. Grep for "Elastic License" / "ELv2" across all .md and .go files,
   replace any references with "MIT".
5. Commit on a branch `chore/license-mit` with message:
   "chore: relicense to MIT for OSS distribution"
6. Do NOT push. Report the diff and the branch name.

Comment discipline: No comments unless the WHY is non-obvious. No
JSDoc. No step-narration comments. No file-header explanations of
the obvious flow.
```

---

## Task 2 — Repo metadata (description, topics, homepage)

**Subagent: none — Manish does this in GitHub UI (15 min).**

Set on https://github.com/Msparihar/stt-go/settings:
- **Description:** `Windows hotkey-to-dictation tool. Hold Right-Alt, speak, release — text appears in the focused window. Whisper / Deepgram / ElevenLabs / Groq backends.`
- **Website:** (leave blank or point to README anchor once demo GIF exists)
- **Topics:** `windows`, `speech-to-text`, `dictation`, `whisper`, `deepgram`, `elevenlabs`, `groq`, `golang`, `system-tray`, `hotkey`, `productivity`
- Enable **Discussions** (Settings → Features).
- Disable Wiki if empty (or seed it later).

---

## Task 3 — GoReleaser release workflow

**Subagent: `general-purpose`. Effort: 1 hr. Depends on: 1.**

```text
Wire .goreleaser.yml at /mnt/c/Users/manis/scripts/stt-go/.goreleaser.yml
into a GitHub Actions workflow that triggers on `v*` tag pushes.

Steps:
1. Read /mnt/c/Users/manis/scripts/stt-go/.goreleaser.yml and confirm
   it targets windows/amd64 only — if it claims other GOOS values,
   strip them (this is a Windows-only app, see all `.go` files have
   `//go:build windows`).
2. Read /mnt/c/Users/manis/scripts/stt-go/.github/workflows/ci.yml for
   style reference.
3. Create /mnt/c/Users/manis/scripts/stt-go/.github/workflows/release.yml
   with:
     - Trigger: `on: push: tags: ['v*']`
     - Runner: `windows-latest`
     - Steps: checkout (fetch-depth 0), setup-go (read version from
       go.mod), `goreleaser/goreleaser-action@v6` running
       `release --clean`
     - Env: GITHUB_TOKEN from secrets
4. Ensure goreleaser archives include: stt-go.exe, README.md, LICENSE,
   config.example.json. Output zip + checksums.txt.
5. Verify YAML parses: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release.yml'))"`.
6. Commit on `ci/release-workflow` branch:
   "ci: add tag-triggered GoReleaser workflow"

Comment discipline: No comments unless the WHY is non-obvious. No
JSDoc. No step-narration comments. No file-header explanations of
the obvious flow.
```

---

## Task 4 — Cut first auto-built release

**Manual (5 min). Depends on: 1, 3.**

```bash
cd /mnt/c/Users/manis/scripts/stt-go
git tag v1.1.0 -m "v1.1.0: Groq + realtime WS backends, MIT relicense"
git push origin v1.1.0
gh run watch  # confirm release.yml workflow succeeds
gh release view v1.1.0  # confirm artifacts attached
```

If the workflow fails: read the run logs, fix `.goreleaser.yml` or `release.yml`, delete the tag (`git tag -d v1.1.0 && git push --delete origin v1.1.0`), retry.

---

## Task 5 — README rewrite

**Subagent: `general-purpose`. Effort: 2 hr. Depends on: 1, 2.**

```text
Rewrite /mnt/c/Users/manis/scripts/stt-go/README.md as a landing page
for a Windows-only OSS dictation tool. The current README is an 80-line
reference card with no install path, no GIF, no badges.

Required structure (in order):
1. Title + 1-line pitch + animated GIF placeholder (markdown comment:
   `<!-- TODO(manish): replace with docs/demo.gif from ScreenToGif -->`).
2. Badge row: CI status, latest release, license (MIT), platform
   (Windows). Use shields.io. Use the actual repo path Msparihar/stt-go.
3. Features: bullets covering 6 backends (Whisper REST + streaming,
   Whisper Realtime WS, Deepgram, ElevenLabs Scribe streaming + batch,
   Groq), hotkey workflow, system tray, clipboard image passthrough,
   keyterm hinting, replacements dictionary.
4. Install:
     a. Recommended: `Download stt-go.exe from Releases` (link)
     b. Coming soon: winget install Msparihar.stt-go
     c. From source: `go build -ldflags '-H windowsgui'` (no
        hardcoded Windows paths in the example)
5. Get API keys: 4 links — OpenAI, Deepgram, ElevenLabs, Groq.
6. First run: `stt-go.exe --setup` walks through key entry.
7. Usage: hold Right-Alt, speak, release. Tray menu to switch backend.
8. Configuration: where config.json lives, what each field does
   (link to config.example.json — read it, don't guess fields).
9. Troubleshooting: SmartScreen "Unknown Publisher" (until code-sign
   lands, task 13–14), mic not detected, auth_error in log.
10. Roadmap: small list. State explicitly "Windows-only by design;
    macOS/Linux are not on the roadmap."
11. License: MIT.
12. Acknowledgements: link to whisper/deepgram/elevenlabs/groq.

Drop ANY language that implies cross-platform support. Read every
.go file's build tag — they all say `//go:build windows`.

Source files to read first:
- /mnt/c/Users/manis/scripts/stt-go/README.md (current)
- /mnt/c/Users/manis/scripts/stt-go/config.example.json
- /mnt/c/Users/manis/scripts/stt-go/config.go (for the field list)
- /mnt/c/Users/manis/scripts/stt-go/tray.go (for backend menu names)
- /mnt/c/Users/manis/scripts/stt-go/main.go (for --setup flag)

Commit on `docs/readme-landing-page` branch:
"docs: rewrite README as landing page with install + usage + badges"

Comment discipline: No comments unless the WHY is non-obvious. No
JSDoc. No step-narration comments. No file-header explanations of
the obvious flow.
```

---

## Task 6 — Demo GIF (manual)

**Manish only (1 hr).** Install ScreenToGif (https://www.screentogif.com — free, OSS). Record:
1. Empty Notepad open.
2. Hold Right-Alt, say one sentence ("hello, this is a test of speech to text"), release.
3. Text appears in Notepad as it's typed.
4. Trim to 8–12 seconds. Export as `docs/demo.gif` (target <2 MB). Commit. Update README to point to the real file.

---

## Task 7 — SECURITY.md / CONTRIBUTING.md / CoC / templates

**Subagent: `general-purpose`. Effort: 1 hr.**

```text
Create five files under /mnt/c/Users/manis/scripts/stt-go/:

1. SECURITY.md — disclose:
   - API keys stored plaintext in %APPDATA%\stt-go\config.json (today).
   - Recommend Credential Manager migration (task 12 in OSS-READINESS).
   - Audio captures land in debug-audio/ + failed-audio/ — local-only,
     never uploaded by the app; user is responsible for clearing them.
   - Reporting: open a private security advisory via GitHub.
   - Supported versions: latest minor.

2. CONTRIBUTING.md — covering:
   - Build prereqs: Go (version from go.mod), Windows 10+
   - Build command (no hardcoded user paths)
   - Run tests: `go test -race ./...`
   - Style: `go vet`, `staticcheck` (CI enforces — check
     .github/workflows/ci.yml).
   - Branch + PR flow, commit format (look at recent git log for
     style — use `git -C /mnt/c/Users/manis/scripts/stt-go log
     --oneline -20`).

3. CODE_OF_CONDUCT.md — Contributor Covenant v2.1, contact email
   manishsparihar2020@gmail.com.

4. .github/ISSUE_TEMPLATE/bug_report.yml — fields: Windows build,
   Go version (if from source), backend selected, log excerpt
   (point to %APPDATA%\stt-go\stt-go.log), repro steps, expected vs
   actual.

5. .github/ISSUE_TEMPLATE/feature_request.yml — fields: problem
   description, proposed solution, alternatives considered.

6. .github/pull_request_template.md — short: what + why, screenshots
   for UI changes, test plan, checkbox for "ran go vet + tests".

DO NOT invent fields. Look at central-backend-host or any other M32
repo for tone reference if helpful, but keep these scoped to an
indie OSS Windows tool — not enterprise process theater.

Commit on `docs/community-files` branch:
"docs: add SECURITY, CONTRIBUTING, CoC, issue + PR templates"

Comment discipline: No comments unless the WHY is non-obvious. No
JSDoc. No step-narration comments. No file-header explanations of
the obvious flow.
```

---

## Task 8 — CHANGELOG + Discussions

**Mostly manual (15 min).** Enable Discussions in repo settings. GoReleaser will auto-generate release notes from commits since the previous tag — no separate CHANGELOG.md file needed if you trust the tag-based notes. If you want a curated file, add `CHANGELOG.md` in Keep-a-Changelog format seeded with v1.0.0 (April) and v1.1.0 entries.

---

## Task 9 — Drop cross-platform claims

**Subagent: `general-purpose`. Effort: 30 min.**

```text
Audit /mnt/c/Users/manis/scripts/stt-go/ for any text that implies
mac/Linux support. The app is Windows-only — every .go file has
`//go:build windows`, audio is waveIn, UI is Direct2D, hotkeys are
Win32 user32 calls.

Steps:
1. grep -inE "macOS|mac os|linux|cross-platform|cross platform" in
   README.md, CLAUDE.md, .goreleaser.yml, all .md files.
2. For each hit, either delete the line, or rephrase to make
   Windows-only explicit.
3. In .goreleaser.yml: ensure builds.goos = ["windows"] only.
4. README roadmap section MUST contain: "Windows-only by design.
   macOS/Linux are not planned — audio capture, hotkeys, and the
   overlay all use Win32 primitives that would require a full
   rewrite per platform."

Commit on `docs/windows-only-honesty` branch:
"docs: explicitly state Windows-only, drop cross-platform hints"

Comment discipline: No comments unless the WHY is non-obvious.
No JSDoc. No step-narration comments. No file-header explanations
of the obvious flow.
```

---

## Task 10 — winget manifest

**Subagent: `general-purpose`. Effort: 2 hr. Depends on: 1, 3, 4 (need a published Release with a stable URL).**

```text
Create a winget package manifest for Msparihar.stt-go and prepare
a PR to microsoft/winget-pkgs.

Steps:
1. Confirm latest release exists: `gh release view --repo Msparihar/stt-go`.
   Get the .zip URL and its SHA256.
2. Generate manifest scaffold:
   `winget create` is interactive — instead, write the three YAML
   files directly under a scratch dir
   /mnt/c/Users/manis/scripts/stt-go/dist/winget/manifests/m/Msparihar/stt-go/<version>/:
     - Msparihar.stt-go.installer.yaml — installer URL, sha256,
       InstallerType: zip, NestedInstallerFiles pointing at
       stt-go.exe, architecture x64.
     - Msparihar.stt-go.locale.en-US.yaml — Description, License (MIT),
       PackageUrl, Tags (same as task 2), ReleaseNotesUrl.
     - Msparihar.stt-go.yaml — version manifest.
3. Validate locally with `winget validate --manifest <dir>` if
   winget is installed on the Windows side (run via cmd.exe).
4. Output the three YAML files + the exact `gh repo fork +
   gh pr create` commands to submit to microsoft/winget-pkgs.
   DO NOT submit the PR — surface the commands for Manish to run.

Comment discipline: No comments unless the WHY is non-obvious.
No JSDoc. No step-narration comments. No file-header explanations
of the obvious flow.
```

---

## Task 11 — scoop manifest

**Subagent: `general-purpose`. Effort: 1 hr. Depends on: 4.**

```text
Create a scoop manifest at
/mnt/c/Users/manis/scripts/stt-go/dist/scoop/stt-go.json.

Fields: version, description, homepage, license (MIT), architecture
.64bit.url (release zip URL), .64bit.hash (sha256), bin (stt-go.exe),
checkver (github regex), autoupdate.

Reference spec: https://github.com/ScoopInstaller/Scoop/wiki/App-Manifests.

After writing: explain to Manish how to either (a) submit to
scoop-extras bucket or (b) host as own bucket repo
github.com/Msparihar/scoop-bucket. Surface the gh commands for
both, do NOT submit.

Commit on `dist/scoop-manifest` branch:
"dist: add scoop manifest for stt-go"

Comment discipline: No comments unless the WHY is non-obvious.
No JSDoc. No step-narration comments. No file-header explanations
of the obvious flow.
```

---

## Task 12 — Windows Credential Manager for keys

**Subagent: `general-purpose`. Effort: 4 hr. Depends on: 1.**

```text
Migrate API key storage from plaintext config.json to Windows
Credential Manager, keeping config.json as a fallback with a runtime
warning.

Current implementation:
- Key load order at /mnt/c/Users/manis/scripts/stt-go/main.go:126-160:
  config.json api_keys.<name> → env var → ~/.env.local file.
- Config struct at config.go:25-32 has the api_keys block.

Target:
1. Insert a NEW first-priority lookup: Windows Credential Manager
   via golang.org/x/sys/windows. Use the `CredRead` /
   `CredWrite` Win32 APIs (the package wraps them as
   `windows.CredRead` / `CredWrite`). Target name format:
   "stt-go:<KEY_NAME>" e.g. "stt-go:OPENAI_API_KEY". Type:
   CRED_TYPE_GENERIC.
2. Update --setup wizard in config.go:156 to offer "save to
   Credential Manager (recommended)" vs "save to config.json".
   Default to Credential Manager.
3. If a key is read from config.json and Credential Manager is
   empty, log a one-time WARN: "[CFG] <KEY> is stored in plaintext
   config.json — run `stt-go.exe --setup` to migrate to Credential
   Manager".
4. Add tests in main_test.go (new file) that mock the credential
   read path. Do NOT call real CredRead in tests.
5. Update SECURITY.md (created in task 7) to reflect Credential
   Manager is now the default.

Verification:
- `go build -ldflags '-H windowsgui'` succeeds.
- `go vet ./...` clean.
- `go test -race ./...` passes (the existing tests must still pass).
- Manual: run `stt-go.exe --setup`, enter test key, confirm it lands
  in Credential Manager (`cmdkey /list` shows "stt-go:OPENAI_API_KEY").

Commit on `feat/credential-manager` branch:
"feat: store API keys in Windows Credential Manager (config.json fallback)"

Comment discipline: No comments unless the WHY is non-obvious.
No JSDoc. No step-narration comments. No file-header explanations
of the obvious flow.
```

---

## Task 13 — Apply for SignPath OSS code-signing

**Manual (1 hr to apply + 1–2 week wait).** Go to https://signpath.io/signpath-foundation. Apply for the OSS Foundation program — they sign open-source Windows binaries for free. Requires: public repo with OSI license (task 1 unblocks this), maintainer identity verification, a build pipeline they can inspect (release.yml from task 3 satisfies this).

While waiting, the README should note "Signed builds coming via SignPath OSS Foundation; until then SmartScreen may warn" in the troubleshooting section.

---

## Task 14 — Wire SignPath into GoReleaser

**Subagent: `general-purpose`. Effort: 1 hr. Depends on: 13 approval.**

```text
Once SignPath approves the OSS application, wire their signing
action into release.yml (created in task 3).

Reference: https://about.signpath.io/documentation/build-system-integration/github-actions

Steps:
1. Read current /mnt/c/Users/manis/scripts/stt-go/.github/workflows/release.yml
   and /mnt/c/Users/manis/scripts/stt-go/.goreleaser.yml.
2. Add a `signpath-pages/signpath-action@v1` step AFTER goreleaser
   produces stt-go.exe but BEFORE the archive is zipped. The action
   uploads the exe, SignPath signs it, returns it. Re-pack the
   signed binary into the goreleaser archive.
3. Secrets needed (Manish adds to repo settings):
   SIGNPATH_API_TOKEN, SIGNPATH_ORG_ID, SIGNPATH_PROJECT_SLUG,
   SIGNPATH_SIGNING_POLICY_SLUG.
4. Update README troubleshooting: remove the "SmartScreen warning"
   note since signed builds will no longer trigger it.

Commit on `ci/signpath` branch:
"ci: code-sign release binaries via SignPath OSS"

Comment discipline: No comments unless the WHY is non-obvious.
No JSDoc. No step-narration comments. No file-header explanations
of the obvious flow.
```

---

## Notes for future Claude sessions

- The repo is at `Msparihar/stt-go` (personal account, not M32 — see CLAUDE-SHARED.md three-identity note). Push from Windows side using `git@github-msparihar:Msparihar/stt-go.git`.
- WSL `gh` CLI is authed as `manishsparihar32` — it CANNOT manage this repo. For `gh` operations, run `gh.exe` from Windows side or use the GitHub API with a PAT for the Msparihar account.
- Free OSS tooling only — no Azure Trusted Signing ($10/mo). SignPath OSS Foundation does the same job for free.
- Don't reintroduce cross-platform claims. The codebase is Win32-bound; any "macOS support" suggestion is a rewrite, not a port.
