//go:build windows

package main

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// localWhisperPython resolves the Python interpreter for the sidecar.
// Priority: config.json local_whisper_python → <exeDir>/sidecar/.venv/Scripts/pythonw.exe → "pythonw" on PATH.
func localWhisperPython() string {
	if appConfig != nil && appConfig.LocalWhisperPython != "" {
		return appConfig.LocalWhisperPython
	}
	exe, _ := os.Executable()
	candidate := filepath.Join(filepath.Dir(exe), "sidecar", ".venv", "Scripts", "pythonw.exe")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return "pythonw"
}

// localWhisperScript resolves the sidecar script path.
// Priority: config.json local_whisper_script → <exeDir>/sidecar/server.py.
func localWhisperScript() string {
	if appConfig != nil && appConfig.LocalWhisperScript != "" {
		return appConfig.LocalWhisperScript
	}
	exe, _ := os.Executable()
	return filepath.Join(filepath.Dir(exe), "sidecar", "server.py")
}

// localWhisperPort extracts the port from the configured transcribe URL so the
// spawned sidecar (WHISPER_PORT) and the port we free agree with what the client
// talks to.
func localWhisperPort() string {
	raw := defaultLocalWhisperURL
	if appConfig != nil && appConfig.LocalWhisperURL != "" {
		raw = appConfig.LocalWhisperURL
	}
	if u, err := url.Parse(raw); err == nil {
		if p := u.Port(); p != "" {
			return p
		}
	}
	return "5111"
}

// freeLocalWhisperPort kills whatever currently owns the sidecar port plus any
// stale server.py instance, so a single fresh sidecar can own the port. This is
// what eliminates the old race between the app and the separate scheduled task.
func freeLocalWhisperPort(port string, log *slog.Logger) {
	// Match the resolved script path (both slash styles) so the stale-process
	// sweep works whether the script lives in sidecar/ or a config-set path.
	script := localWhisperScript()
	esc := func(s string) string { return strings.ReplaceAll(s, "'", "''") }
	back := esc(strings.ReplaceAll(script, "/", `\`))
	fwd := esc(strings.ReplaceAll(script, `\`, "/"))
	ps := `$p=` + port + `;` +
		`Get-NetTCPConnection -LocalPort $p -State Listen -ErrorAction SilentlyContinue |` +
		` ForEach-Object { Stop-Process -Id $_.OwningProcess -Force -ErrorAction SilentlyContinue };` +
		`Get-CimInstance Win32_Process -Filter "Name='pythonw.exe' OR Name='python.exe'" |` +
		` Where-Object { $_.CommandLine -like '*` + back + `*' -or $_.CommandLine -like '*` + fwd + `*' } |` +
		` ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", ps)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Run(); err != nil {
		log.Warn("[LOCAL] port cleanup returned error (continuing)", "port", port, "err", err)
	}
	time.Sleep(400 * time.Millisecond)
}

// spawnLocalWhisperSidecar launches server.py detached, with stdout/stderr
// redirected to server.out.log in the same directory as the script.
func spawnLocalWhisperSidecar(port string, log *slog.Logger) error {
	python, script := localWhisperPython(), localWhisperScript()
	cmd := exec.Command(python, script)
	cmd.Dir = filepath.Dir(script)
	cmd.Env = append(os.Environ(), "WHISPER_PORT="+port)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW

	logPath := filepath.Join(filepath.Dir(script), "server.out.log")
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		cmd.Stdout, cmd.Stderr = f, f
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	log.Info("[LOCAL] sidecar spawned", "pid", cmd.Process.Pid, "port", port, "python", python)
	_ = cmd.Process.Release()
	return nil
}

// ensureLocalWhisperSidecar guarantees a healthy sidecar is running, then warms
// the model. Reuses an already-healthy server; otherwise frees the port and
// spawns a fresh one. Runs in the background so callers never block.
func ensureLocalWhisperSidecar(log *slog.Logger) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		reachable, _ := localWhisperHealth(ctx)
		cancel()
		if reachable {
			log.Info("[LOCAL] sidecar already healthy, reusing")
			loadLocalWhisper(log)
			return
		}
		respawnLocalWhisper(log)
	}()
}

// restartLocalWhisperSidecar unconditionally frees the port and respawns the
// sidecar, even if one appears reachable — used by the tray "retry" action when
// the model is wedged. Runs in the background so the tray click returns instantly.
func restartLocalWhisperSidecar(log *slog.Logger) {
	go respawnLocalWhisper(log)
}

// respawnLocalWhisper frees the port, spawns a fresh sidecar, waits for health,
// then warms the model. Synchronous; callers run it in a goroutine.
func respawnLocalWhisper(log *slog.Logger) {
	port := localWhisperPort()
	freeLocalWhisperPort(port, log)
	if err := spawnLocalWhisperSidecar(port, log); err != nil {
		log.Error("[LOCAL] failed to spawn sidecar", "err", err)
		return
	}

	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		c, ccancel := context.WithTimeout(context.Background(), 3*time.Second)
		reachable, _ := localWhisperHealth(c)
		ccancel()
		if reachable {
			log.Info("[LOCAL] sidecar is up")
			loadLocalWhisper(log)
			return
		}
		time.Sleep(600 * time.Millisecond)
	}
	log.Error("[LOCAL] sidecar did not become healthy in time")
}
