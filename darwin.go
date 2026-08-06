//go:build darwin

package main

/*
#cgo LDFLAGS: -framework ApplicationServices -framework CoreFoundation
#include <ApplicationServices/ApplicationServices.h>

// kVK_RightOption = 0x3D (61)
static int sttHotkeyDown() {
	return CGEventSourceKeyState(kCGEventSourceStateCombinedSessionState, 61) ? 1 : 0;
}

// Types a UTF-16 string by attaching it to a synthetic keyboard event.
// Keycode 0 is irrelevant — the unicode payload is what gets inserted.
static void sttTypeUTF16(const UniChar *chars, long len) {
	CGEventRef down = CGEventCreateKeyboardEvent(NULL, 0, true);
	CGEventKeyboardSetUnicodeString(down, len, chars);
	CGEventPost(kCGHIDEventTap, down);
	CFRelease(down);
	CGEventRef up = CGEventCreateKeyboardEvent(NULL, 0, false);
	CGEventKeyboardSetUnicodeString(up, len, chars);
	CGEventPost(kCGHIDEventTap, up);
	CFRelease(up);
}

// kVK_Delete = 51 (backspace)
static void sttPressBackspace() {
	CGEventRef down = CGEventCreateKeyboardEvent(NULL, 51, true);
	CGEventPost(kCGHIDEventTap, down);
	CFRelease(down);
	CGEventRef up = CGEventCreateKeyboardEvent(NULL, 51, false);
	CGEventPost(kCGHIDEventTap, up);
	CFRelease(up);
}
*/
import "C"

import (
	"log/slog"
	"time"
	"unicode/utf16"
	"unsafe"
)

// ── Hotkey + foreground window ─────────────────────────────────────

// hotkeyDown reports whether the push-to-talk key (Right Option) is held.
// Needs the Input Monitoring permission on macOS 10.15+.
func hotkeyDown() bool {
	return C.sttHotkeyDown() != 0
}

// captureForegroundWindow is a no-op on macOS: synthetic keyboard events go
// to whatever app is focused, so there is no window handle to save/restore.
func captureForegroundWindow() uintptr { return 0 }

// waitForRightAltRelease polls until Right Option is released so the held
// modifier can't combine with the synthetic keystrokes.
func waitForRightAltRelease() {
	for hotkeyDown() {
		time.Sleep(10 * time.Millisecond)
	}
}

// ── Text typer (CGEvent + unicode payload) ─────────────────────────

// Max UTF-16 units per synthetic event. Apps drop long payloads; 20 is the
// commonly safe chunk size.
const typeChunk = 20

func postUnicode(text string, log *slog.Logger) {
	units := utf16.Encode([]rune(text))
	for i := 0; i < len(units); i += typeChunk {
		end := min(i+typeChunk, len(units))
		chunk := units[i:end]
		C.sttTypeUTF16((*C.UniChar)(unsafe.Pointer(&chunk[0])), C.long(len(chunk)))
		time.Sleep(2 * time.Millisecond) // let the event queue drain
	}
}

// typeText types the given text into the focused app. Waits for the hotkey
// to be released first. Needs the Accessibility permission.
func typeText(text string, _ uintptr, log *slog.Logger) {
	if len(text) > 80 {
		log.Info("[TYPE] typeText: will type", "chars", len(text), "text", text[:80]+"...")
	} else {
		log.Info("[TYPE] typeText: will type", "chars", len(text), "text", text)
	}

	log.Info("[TYPE] typeText: waiting for RightOption release")
	waitStart := time.Now()
	waitForRightAltRelease()
	log.Info("[TYPE] typeText: RightOption released", "waitDuration", time.Since(waitStart).Round(time.Millisecond))

	// Pre-type delay to let focus settle
	const preTypeDelay = 150 * time.Millisecond
	time.Sleep(preTypeDelay)

	if text == "" {
		return
	}
	postUnicode(text, log)
	log.Info("[TYPE] typeText: completed", "chars", len([]rune(text)))
}

// sendBackspaces sends n backspace key events to the focused app.
func sendBackspaces(n int, _ uintptr, log *slog.Logger) {
	for i := 0; i < n; i++ {
		C.sttPressBackspace()
		time.Sleep(2 * time.Millisecond)
	}
}

// typeRunes types runes mid-recording (streaming mode) — no release wait.
func typeRunes(runes []rune, _ uintptr, log *slog.Logger) {
	if len(runes) == 0 {
		return
	}
	postUnicode(string(runes), log)
}
