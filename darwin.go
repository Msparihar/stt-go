//go:build darwin

package main

/*
#cgo LDFLAGS: -framework ApplicationServices -framework CoreFoundation -framework IOKit
#include <ApplicationServices/ApplicationServices.h>
#include <IOKit/hidsystem/IOHIDLib.h>

// Input Monitoring: 0 = granted, 1 = denied, 2 = not yet asked
static int sttInputMonitoringStatus() {
	return (int)IOHIDCheckAccess(kIOHIDRequestTypeListenEvent);
}

// Pops the system prompt and adds this app to the Input Monitoring list.
static int sttRequestInputMonitoring() {
	return IOHIDRequestAccess(kIOHIDRequestTypeListenEvent) ? 1 : 0;
}

static int sttAccessibilityTrusted() {
	return AXIsProcessTrusted() ? 1 : 0;
}

// Shows the system dialog pointing the user at Accessibility settings.
static int sttAccessibilityPrompt() {
	const void *keys[] = { kAXTrustedCheckOptionPrompt };
	const void *values[] = { kCFBooleanTrue };
	CFDictionaryRef opts = CFDictionaryCreate(kCFAllocatorDefault, keys, values, 1,
		&kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	int trusted = AXIsProcessTrustedWithOptions(opts) ? 1 : 0;
	CFRelease(opts);
	return trusted;
}

// ── Hotkey event tap ────────────────────────────────────────────────
// CGEventSourceKeyState is unreliable here: Karabiner re-posts right Option
// with a different keycode in the state table. A flagsChanged event tap sees
// the true keycode (61), so we track key state from the event stream instead.

extern void goHotkeyFlags(long long keycode, unsigned long long flags);

static CGEventRef sttHotkeyTapCB(CGEventTapProxy proxy, CGEventType type, CGEventRef event, void *info) {
	if (type == kCGEventFlagsChanged) {
		goHotkeyFlags(
			CGEventGetIntegerValueField(event, kCGKeyboardEventKeycode),
			(unsigned long long)CGEventGetFlags(event));
	} else if (type == kCGEventTapDisabledByTimeout || type == kCGEventTapDisabledByUserInput) {
		CGEventTapEnable((CFMachPortRef)info, true);
	}
	return event;
}

static CFMachPortRef sttTap = NULL;

// Creates the listen-only tap on the calling thread's run loop.
// Returns 1 on success, 0 if the tap was refused (Input Monitoring missing).
static int sttStartHotkeyTap() {
	sttTap = CGEventTapCreate(kCGSessionEventTap, kCGHeadInsertEventTap,
		kCGEventTapOptionListenOnly, CGEventMaskBit(kCGEventFlagsChanged),
		sttHotkeyTapCB, NULL);
	if (!sttTap) return 0;
	CFRunLoopSourceRef src = CFMachPortCreateRunLoopSource(kCFAllocatorDefault, sttTap, 0);
	CFRunLoopAddSource(CFRunLoopGetCurrent(), src, kCFRunLoopCommonModes);
	CGEventTapEnable(sttTap, true);
	return 1;
}

static void sttRunHotkeyLoop() {
	CFRunLoopRun();
}

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
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"
	"unicode/utf16"
	"unsafe"
)

// rightOptionHeld is fed by the flagsChanged event tap.
var rightOptionHeld atomic.Bool

// tapRunning reports whether the event tap started; if not, hotkeyDown falls
// back to CGEventSourceKeyState polling.
var tapRunning atomic.Bool

const (
	kVKRightOption = 61
	maskAlternate  = 0x00080000 // kCGEventFlagMaskAlternate

	hotkeyName = "Right Option"
)

//export goHotkeyFlags
func goHotkeyFlags(keycode C.longlong, flags C.ulonglong) {
	if keycode == kVKRightOption {
		rightOptionHeld.Store(uint64(flags)&maskAlternate != 0)
	}
}

// startHotkeyTap runs the event tap on a dedicated OS thread.
func startHotkeyTap(log *slog.Logger) {
	ready := make(chan bool, 1)
	go func() {
		runtime.LockOSThread()
		ok := C.sttStartHotkeyTap() == 1
		ready <- ok
		if ok {
			C.sttRunHotkeyLoop() // never returns
		}
	}()
	if <-ready {
		tapRunning.Store(true)
		log.Info("[PERM] hotkey event tap active (Right Option, keycode 61)")
	} else {
		log.Error("[PERM] hotkey event tap REFUSED — falling back to key-state polling. Grant Input Monitoring and restart.")
	}
}

// ── Startup permission checks ──────────────────────────────────────

// platformStartupChecks logs macOS permission status and requests Input
// Monitoring if it was never asked. Both permissions attach to the app that
// launched the binary (e.g. the terminal), not the binary itself.
func platformStartupChecks(log *slog.Logger) {
	switch C.sttInputMonitoringStatus() {
	case 0:
		log.Info("[PERM] Input Monitoring: granted — hotkey will work")
	case 1:
		log.Error("[PERM] Input Monitoring: DENIED — hotkey will NOT work. Enable your terminal in System Settings → Privacy & Security → Input Monitoring, then relaunch the terminal.")
	default:
		log.Warn("[PERM] Input Monitoring: not determined — requesting now (watch for a system prompt)")
		C.sttRequestInputMonitoring()
	}

	if C.sttAccessibilityTrusted() == 1 {
		log.Info("[PERM] Accessibility: granted — typing will work")
	} else {
		log.Error("[PERM] Accessibility: NOT granted — transcribed text cannot be typed. Requesting now (watch for a system dialog).")
		C.sttAccessibilityPrompt()
	}

	startHotkeyTap(log)
}

// appDataDir is where config, logs, and audio dumps live:
// ~/Library/Application Support/STT-Go.
func appDataDir() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, "Library", "Application Support", "STT-Go")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// ── Hotkey + foreground window ─────────────────────────────────────

// hotkeyDown reports whether the push-to-talk key (Right Option) is held.
// Needs the Input Monitoring permission on macOS 10.15+.
func hotkeyDown() bool {
	if tapRunning.Load() {
		return rightOptionHeld.Load()
	}
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
