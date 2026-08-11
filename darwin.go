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
// CGEventSourceKeyState is unreliable here: Karabiner re-posts modifier keys
// with different keycodes in the state table. A flagsChanged event tap sees
// the true keycode, so we track key state from the event stream instead.

// evType: 1 = flagsChanged, 2 = keyDown, 3 = keyUp
extern void goHotkeyFlags(int evType, long long keycode, unsigned long long flags);

// Marker stamped on every keyboard event this app posts (typing, backspace),
// so the hotkey tap can ignore its own output: typed events carry no modifier
// flags, and treating them as real input made the app think the hotkey was
// released whenever it typed while the user was already holding for the next
// dictation.
#define STT_EVENT_MARKER 0x53545447

static CGEventRef sttHotkeyTapCB(CGEventTapProxy proxy, CGEventType type, CGEventRef event, void *info) {
	if (CGEventGetIntegerValueField(event, kCGEventSourceUserData) == STT_EVENT_MARKER) {
		return event;
	}
	if (type == kCGEventFlagsChanged || type == kCGEventKeyDown || type == kCGEventKeyUp) {
		goHotkeyFlags(
			type == kCGEventFlagsChanged ? 1 : (type == kCGEventKeyDown ? 2 : 3),
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
// keyDown/keyUp are tapped alongside flagsChanged because synthetic modifier
// presses (Logi Options+ button shortcuts) arrive as keyDown with keycode
// 65535 and only the modifier flag set — no flagsChanged is posted.
static int sttStartHotkeyTap() {
	sttTap = CGEventTapCreate(kCGSessionEventTap, kCGHeadInsertEventTap,
		kCGEventTapOptionListenOnly,
		CGEventMaskBit(kCGEventFlagsChanged) | CGEventMaskBit(kCGEventKeyDown) | CGEventMaskBit(kCGEventKeyUp),
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

static int sttModifierDown(unsigned long long mask) {
	return (CGEventSourceFlagsState(kCGEventSourceStateCombinedSessionState) & mask) ? 1 : 0;
}

// Types a UTF-16 string by attaching it to a synthetic keyboard event.
// Keycode 0 is irrelevant — the unicode payload is what gets inserted.
static void sttTypeUTF16(const UniChar *chars, long len) {
	CGEventRef down = CGEventCreateKeyboardEvent(NULL, 0, true);
	CGEventKeyboardSetUnicodeString(down, len, chars);
	CGEventSetIntegerValueField(down, kCGEventSourceUserData, STT_EVENT_MARKER);
	CGEventPost(kCGHIDEventTap, down);
	CFRelease(down);
	CGEventRef up = CGEventCreateKeyboardEvent(NULL, 0, false);
	CGEventKeyboardSetUnicodeString(up, len, chars);
	CGEventSetIntegerValueField(up, kCGEventSourceUserData, STT_EVENT_MARKER);
	CGEventPost(kCGHIDEventTap, up);
	CFRelease(up);
}

// kVK_Delete = 51 (backspace)
static void sttPressBackspace() {
	CGEventRef down = CGEventCreateKeyboardEvent(NULL, 51, true);
	CGEventSetIntegerValueField(down, kCGEventSourceUserData, STT_EVENT_MARKER);
	CGEventPost(kCGHIDEventTap, down);
	CFRelease(down);
	CGEventRef up = CGEventCreateKeyboardEvent(NULL, 51, false);
	CGEventSetIntegerValueField(up, kCGEventSourceUserData, STT_EVENT_MARKER);
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
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

// hotkeyHeld is fed by the event tap.
var hotkeyHeld atomic.Bool

// tapRunning reports whether the event tap started; if not, hotkeyDown falls
// back to CGEventSourceFlagsState polling.
var tapRunning atomic.Bool

const (
	maskShift     = 0x00020000 // kCGEventFlagMaskShift
	maskControl   = 0x00040000 // kCGEventFlagMaskControl
	maskAlternate = 0x00080000 // kCGEventFlagMaskAlternate
	maskCommand   = 0x00100000 // kCGEventFlagMaskCommand
	maskFn        = 0x00800000 // kCGEventFlagMaskSecondaryFn
)

// hotkeySpec describes a push-to-talk modifier. keycodes empty = side-agnostic:
// state follows the flag bit on any event, which also catches synthetic
// presses (keycode 65535) from Logi Options+ style button shortcuts.
// Side-specific keys must match a real keycode on flagsChanged, so synthetic
// events can't trigger them.
type hotkeySpec struct {
	mask     uint64
	keycodes []int64
	name     string
}

var hotkeySpecs = map[string]hotkeySpec{
	"ctrl":         {maskControl, nil, "Ctrl"},
	"left_ctrl":    {maskControl, []int64{59}, "Left Ctrl"},
	"right_ctrl":   {maskControl, []int64{62}, "Right Ctrl"},
	"option":       {maskAlternate, nil, "Option"},
	"left_option":  {maskAlternate, []int64{58}, "Left Option"},
	"right_option": {maskAlternate, []int64{61}, "Right Option"},
	"cmd":          {maskCommand, nil, "Cmd"},
	"left_cmd":     {maskCommand, []int64{55}, "Left Cmd"},
	"right_cmd":    {maskCommand, []int64{54}, "Right Cmd"},
	"shift":        {maskShift, nil, "Shift"},
	"left_shift":   {maskShift, []int64{56}, "Left Shift"},
	"right_shift":  {maskShift, []int64{60}, "Right Shift"},
	"fn":           {maskFn, nil, "Fn"},
}

// Active hotkey, set by initHotkey from config before the tap starts.
var (
	hotkeyMask  uint64 = maskControl
	hotkeyCodes []int64
	hotkeyName  = "Ctrl"
	hotkeyLog   *slog.Logger
)

// initHotkey resolves config.json's "hotkey" field (default "ctrl").
func initHotkey(log *slog.Logger) {
	hotkeyLog = log
	key := "ctrl"
	if appConfig != nil && appConfig.Hotkey != "" {
		key = strings.ToLower(appConfig.Hotkey)
	}
	spec, ok := hotkeySpecs[key]
	if !ok {
		names := make([]string, 0, len(hotkeySpecs))
		for n := range hotkeySpecs {
			names = append(names, n)
		}
		sort.Strings(names)
		log.Error("[CFG] unknown hotkey in config.json — falling back to ctrl",
			"hotkey", key, "valid", strings.Join(names, ", "))
		spec = hotkeySpecs["ctrl"]
	}
	hotkeyMask, hotkeyCodes, hotkeyName = spec.mask, spec.keycodes, spec.name
	tapToggleOn = appConfig == nil || appConfig.TapToggle == nil || *appConfig.TapToggle
	log.Info("[CFG] push-to-talk hotkey", "key", hotkeyName, "tap_toggle", tapToggleOn)
}

// toggleLatched flips on a bare tap of the hotkey (down→up under
// tapToggleMaxHold with no other key pressed in between), so a gesture-button
// click or a quick Ctrl tap toggles recording instead of needing a hold.
// A tap while latched unlatches (stops the recording). Modifier+key combos
// (Ctrl+C etc.) never count as taps.
var toggleLatched atomic.Bool

const tapToggleMaxHold = 350 * time.Millisecond

// releaseGrace absorbs spurious sub-250ms "key up" glitches from Logi
// Options+ synthetic modifiers; a real release just ends 250ms later.
const releaseGrace = 250 * time.Millisecond

// Tap-detection state. Only touched from the event-tap thread.
var (
	tapDownAt    time.Time
	tapSawOther  bool
	tapWasHeld   bool
	tapToggleOn  bool // from config, set by initHotkey
)

//export goHotkeyFlags
func goHotkeyFlags(evType C.int, keycode C.longlong, flags C.ulonglong) {
	held := uint64(flags)&hotkeyMask != 0
	if len(hotkeyCodes) > 0 {
		// Side-specific key: only its own flagsChanged events change state.
		if evType != 1 {
			if evType == 2 {
				tapSawOther = true
			}
			return
		}
		match := false
		for _, kc := range hotkeyCodes {
			if int64(keycode) == kc {
				match = true
				break
			}
		}
		if !match {
			return
		}
	} else if evType == 2 && int64(keycode) != 65535 && held {
		// Real key pressed while the modifier is down: this is a combo, not a tap.
		tapSawOther = true
	}

	if tapToggleOn {
		if held && !tapWasHeld {
			tapDownAt = time.Now()
			tapSawOther = false
		} else if !held && tapWasHeld {
			if !tapSawOther && time.Since(tapDownAt) < tapToggleMaxHold {
				toggleLatched.Store(!toggleLatched.Load())
			}
		}
		tapWasHeld = held
	}
	// Log press/release transitions with their raw event so an early release
	// can be traced to its source: keycode 59/62 = real keyboard Ctrl,
	// 65535 = synthetic (Logi Options+), evType 1 with other keycodes = a
	// bare flags drop.
	if held != hotkeyHeld.Load() && hotkeyLog != nil {
		hotkeyLog.Info("[KEY] hotkey transition",
			"held", held, "evType", int(evType), "keycode", int64(keycode),
			"flags", "0x"+strconv.FormatUint(uint64(flags), 16))
	}
	hotkeyHeld.Store(held)
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
		log.Info("[PERM] hotkey event tap active", "hotkey", hotkeyName)
	} else {
		log.Error("[PERM] hotkey event tap REFUSED — falling back to key-state polling. Grant Input Monitoring and restart.")
	}
}

// ── Startup permission checks ──────────────────────────────────────

// platformStartupChecks logs macOS permission status and requests Input
// Monitoring if it was never asked. Both permissions attach to the app that
// launched the binary (e.g. the terminal), not the binary itself.
func platformStartupChecks(log *slog.Logger) {
	initHotkey(log)
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

// instanceLock holds the single-instance file lock for the process lifetime.
var instanceLock *os.File

// acquireSingleInstance takes an exclusive lock on a file in the data dir.
// Returns false if another STT-Go already holds it.
func acquireSingleInstance() bool {
	f, err := os.OpenFile(filepath.Join(appDataDir(), "stt-go.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return true // can't lock — don't block startup over it
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return false
	}
	instanceLock = f
	return true
}

// ── Hotkey + foreground window ─────────────────────────────────────

// hotkeyDown reports whether the configured push-to-talk modifier is held.
// Needs the Input Monitoring permission on macOS 10.15+.
func hotkeyDown() bool {
	if tapRunning.Load() {
		return hotkeyHeld.Load() || toggleLatched.Load()
	}
	return C.sttModifierDown(C.ulonglong(hotkeyMask)) != 0
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
