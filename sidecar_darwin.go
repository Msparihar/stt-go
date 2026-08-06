//go:build darwin

package main

import (
	"log/slog"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Sidecar venv layout + interpreter defaults (macOS).
const (
	venvBinDir    = "bin"
	venvPython    = "python3"
	defaultPython = "python3"
)

// sidecarProcAttr detaches the sidecar into its own session so it survives
// the app exiting and never grabs the terminal.
func sidecarProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// freeLocalWhisperPort kills whatever owns the sidecar port plus any stale
// server.py instance, so a single fresh sidecar can own the port.
func freeLocalWhisperPort(port string, log *slog.Logger) {
	if out, err := exec.Command("lsof", "-ti", "tcp:"+port).Output(); err == nil {
		for _, pid := range strings.Fields(string(out)) {
			_ = exec.Command("kill", "-9", pid).Run()
		}
	}
	_ = exec.Command("pkill", "-9", "-f", localWhisperScript()).Run()
	time.Sleep(400 * time.Millisecond)
}
