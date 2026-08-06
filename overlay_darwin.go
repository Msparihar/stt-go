//go:build darwin

package main

import "log/slog"

// waveOverlay is a no-op on macOS for now — recording state is shown via the
// menu bar icon instead. The Direct2D waveform overlay is Windows-only.
type waveOverlay struct {
	log *slog.Logger
}

func newWaveOverlay(log *slog.Logger) *waveOverlay {
	log.Info("[OVERLAY] macOS: overlay disabled, state shown in menu bar")
	return &waveOverlay{log: log}
}

func (w *waveOverlay) show()             {}
func (w *waveOverlay) hide()             {}
func (w *waveOverlay) showTranscribing() {}
func (w *waveOverlay) pushAudio([]byte)  {}
