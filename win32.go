//go:build windows

package main

import (
	"fmt"
	"log/slog"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ── Win32-only constants ───────────────────────────────────────────

const (
	vkRMenu    = 0xA5
	waveMapper = 0xFFFFFFFF
	wavFmtPCM  = 1
	cbEvent    = 0x00050000
	whdrDone   = 0x00000001
	inputKbd   = 1
	kfUnicode  = 0x0004
	kfKeyup    = 0x0002
)

// ── Windows DLL procs ──────────────────────────────────────────────

var (
	user32 = windows.NewLazyDLL("user32.dll")
	winmm  = windows.NewLazyDLL("winmm.dll")
	d2d1   = windows.NewLazyDLL("d2d1.dll")
	gdi32  = windows.NewLazyDLL("gdi32.dll")

	pGetAsyncKey         = user32.NewProc("GetAsyncKeyState")
	pSendInput           = user32.NewProc("SendInput")
	pGetForegroundWindow = user32.NewProc("GetForegroundWindow")
	pSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	pRegisterClassExW    = user32.NewProc("RegisterClassExW")
	pCreateWindowExW     = user32.NewProc("CreateWindowExW")
	pShowWindow          = user32.NewProc("ShowWindow")
	pDefWindowProcW      = user32.NewProc("DefWindowProcW")
	pGetMessageW         = user32.NewProc("GetMessageW")
	pTranslateMessage    = user32.NewProc("TranslateMessage")
	pDispatchMessageW    = user32.NewProc("DispatchMessageW")
	pPostMessageW        = user32.NewProc("PostMessageW")
	pSetLayeredWndAttr   = user32.NewProc("SetLayeredWindowAttributes")
	pSystemParamInfo     = user32.NewProc("SystemParametersInfoW")
	pInvalidateRect      = user32.NewProc("InvalidateRect")
	pBeginPaint          = user32.NewProc("BeginPaint")
	pEndPaint            = user32.NewProc("EndPaint")
	pSetWindowRgn        = user32.NewProc("SetWindowRgn")

	pWaveInOpen        = winmm.NewProc("waveInOpen")
	pWaveInClose       = winmm.NewProc("waveInClose")
	pWaveInPrepHdr     = winmm.NewProc("waveInPrepareHeader")
	pWaveInUnprepHdr   = winmm.NewProc("waveInUnprepareHeader")
	pWaveInAddBuf      = winmm.NewProc("waveInAddBuffer")
	pWaveInStart       = winmm.NewProc("waveInStart")
	pWaveInStop        = winmm.NewProc("waveInStop")
	pWaveInReset       = winmm.NewProc("waveInReset")
	pWaveInGetNumDevs  = winmm.NewProc("waveInGetNumDevs")
	pWaveInGetDevCapsW = winmm.NewProc("waveInGetDevCapsW")

	pMoveWindow         = user32.NewProc("MoveWindow")
	pCreateRoundRectRgn = gdi32.NewProc("CreateRoundRectRgn")
	pD2D1CreateFactory  = d2d1.NewProc("D2D1CreateFactory")
)

// platformStartupChecks is a no-op on Windows — no TCC-style permissions.
func platformStartupChecks(_ *slog.Logger) {}

// setTrayStateTitle is a no-op on Windows — the colored tray icon already
// shows the recording state.
func setTrayStateTitle(_ trayState) {}

// ── Hotkey + foreground window ─────────────────────────────────────

// hotkeyDown reports whether the push-to-talk key (Right Alt) is held.
func hotkeyDown() bool {
	st, _, _ := pGetAsyncKey.Call(vkRMenu)
	return int16(st) < 0
}

// captureForegroundWindow returns the current foreground window handle so it
// can be restored before typing.
func captureForegroundWindow() uintptr {
	hwnd, _, _ := pGetForegroundWindow.Call()
	return hwnd
}

// ── Text typer (SendInput + KEYEVENTF_UNICODE) ────────────────────

type kbInput struct {
	typ       uint32
	_p0       uint32
	vk        uint16
	scan      uint16
	flags     uint32
	time      uint32
	_p1       uint32
	extraInfo uintptr
	_p2       uint64
}

// waitForRightAltRelease polls until Right Alt (VK_RMENU) is released.
// This prevents SendInput from being eaten by the OS when the hotkey
// modifier is still physically held.
func waitForRightAltRelease() {
	for {
		if !hotkeyDown() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// typeText types the given text into the foreground window using SendInput
// with KEYEVENTF_UNICODE. It saves and restores the target window, waits
// for modifier keys to be released, and checks SendInput return values.
func typeText(text string, targetHwnd uintptr, log *slog.Logger) {
	if len(text) > 80 {
		log.Info("[TYPE] typeText: will type", "chars", len(text), "text", text[:80]+"...")
	} else {
		log.Info("[TYPE] typeText: will type", "chars", len(text), "text", text)
	}

	// Wait for Right Alt to be released so SendInput isn't swallowed
	log.Info("[TYPE] typeText: waiting for RightAlt release")
	waitStart := time.Now()
	waitForRightAltRelease()
	log.Info("[TYPE] typeText: RightAlt released", "waitDuration", time.Since(waitStart).Round(time.Millisecond))

	// Restore the window that was focused when recording started
	if targetHwnd != 0 {
		currentHwnd, _, _ := pGetForegroundWindow.Call()
		if currentHwnd != targetHwnd {
			log.Info("[TYPE] typeText: calling SetForegroundWindow", "target", fmt.Sprintf("0x%X", targetHwnd), "current", fmt.Sprintf("0x%X", currentHwnd))
			pSetForegroundWindow.Call(targetHwnd)
			time.Sleep(50 * time.Millisecond) // let window activate
		}
	}

	// Pre-type delay to let focus settle
	const preTypeDelay = 150 * time.Millisecond
	log.Info("[TYPE] typeText: pre-type delay", "delay", preTypeDelay)
	time.Sleep(preTypeDelay)

	runes := []rune(text)
	inputs := make([]kbInput, 0, 2*len(runes))
	for _, ch := range runes {
		inputs = append(inputs,
			kbInput{typ: inputKbd, scan: uint16(ch), flags: kfUnicode},
			kbInput{typ: inputKbd, scan: uint16(ch), flags: kfUnicode | kfKeyup},
		)
	}

	if len(inputs) == 0 {
		return
	}

	ret, _, _ := pSendInput.Call(uintptr(len(inputs)), uintptr(unsafe.Pointer(&inputs[0])), unsafe.Sizeof(inputs[0]))
	if ret == 0 {
		log.Error("[TYPE] typeText: SendInput failed", "chars", len(runes))
	} else {
		log.Info("[TYPE] typeText: completed successfully", "chars", len(runes))
	}
}

// sendBackspaces sends n backspace key events to the target window.
func sendBackspaces(n int, targetHwnd uintptr, log *slog.Logger) {
	if n <= 0 {
		return
	}
	const vkBack = 0x08
	inputs := make([]kbInput, 0, n*2)
	for i := 0; i < n; i++ {
		inputs = append(inputs,
			kbInput{typ: inputKbd, vk: vkBack, flags: 0},
			kbInput{typ: inputKbd, vk: vkBack, flags: kfKeyup},
		)
	}
	if targetHwnd != 0 {
		pSetForegroundWindow.Call(targetHwnd)
	}
	ret, _, _ := pSendInput.Call(uintptr(len(inputs)), uintptr(unsafe.Pointer(&inputs[0])), unsafe.Sizeof(inputs[0]))
	if ret == 0 {
		log.Error("[RT-WS] sendBackspaces: SendInput failed", "n", n)
	}
}

// typeRunes sends unicode key events for a slice of runes. Reuses the same
// pattern as typeText but without the RightAlt-release wait
// (we're mid-recording, the key is still held).
func typeRunes(runes []rune, targetHwnd uintptr, log *slog.Logger) {
	if len(runes) == 0 {
		return
	}
	inputs := make([]kbInput, 0, len(runes)*2)
	for _, ch := range runes {
		inputs = append(inputs,
			kbInput{typ: inputKbd, scan: uint16(ch), flags: kfUnicode},
			kbInput{typ: inputKbd, scan: uint16(ch), flags: kfUnicode | kfKeyup},
		)
	}
	if targetHwnd != 0 {
		pSetForegroundWindow.Call(targetHwnd)
	}
	ret, _, _ := pSendInput.Call(uintptr(len(inputs)), uintptr(unsafe.Pointer(&inputs[0])), unsafe.Sizeof(inputs[0]))
	if ret == 0 {
		log.Error("[RT-WS] typeRunes: SendInput failed", "runes", len(runes))
	}
}
