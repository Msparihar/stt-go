//go:build windows

package main

import (
	"log/slog"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Sidecar venv layout + interpreter defaults (Windows).
const (
	venvBinDir    = "Scripts"
	venvPython    = "pythonw.exe"
	defaultPython = "pythonw"
)

// sidecarProcAttr hides the console window of the spawned sidecar.
func sidecarProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
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
