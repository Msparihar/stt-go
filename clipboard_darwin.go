//go:build darwin

package main

import (
	"context"
	"log/slog"
)

// runClipboardHotkey is Windows-only (Ctrl+Shift+V paste-path helper).
// No-op on macOS.
func runClipboardHotkey(ctx context.Context, log *slog.Logger) {
	<-ctx.Done()
}
