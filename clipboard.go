//go:build windows

package main

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ── Clipboard constants ──────────────────────────────────────────

const (
	cfBitmap = 2
	cfDIB    = 8
	cfDIBV5  = 17
	cfHDROP  = 15

	modCtrl  = 0x0002
	modShift = 0x0004
	vkV      = 0x56

	wmHotkey = 0x0312
)

// ── Clipboard DLL procs ──────────────────────────────────────────

var (
	kernel32 = windows.NewLazyDLL("kernel32.dll")
	shell32  = windows.NewLazyDLL("shell32.dll")

	pOpenClipboard       = user32.NewProc("OpenClipboard")
	pCloseClipboard      = user32.NewProc("CloseClipboard")
	pGetClipboardData    = user32.NewProc("GetClipboardData")
	pIsClipboardFmtAvail = user32.NewProc("IsClipboardFormatAvailable")
	pRegisterHotKey      = user32.NewProc("RegisterHotKey")
	pUnregisterHotKey    = user32.NewProc("UnregisterHotKey")

	pGlobalLock   = kernel32.NewProc("GlobalLock")
	pGlobalUnlock = kernel32.NewProc("GlobalUnlock")

	pDragQueryFileW = shell32.NewProc("DragQueryFileW")

	pPeekMessageW = user32.NewProc("PeekMessageW")
)

// ── BITMAPINFOHEADER ─────────────────────────────────────────────

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

// validateDIBBounds checks if DIB dimensions are valid.
func validateDIBBounds(width, height int32) error {
	w := int(width)
	h := int(height)
	if h < 0 {
		h = -h
	}
	if w <= 0 || h <= 0 || w > 32768 || h > 32768 || int64(w)*int64(h) > 100_000_000 {
		return fmt.Errorf("dib dimensions out of bounds: %dx%d", int(width), int(height))
	}
	return nil
}

// ── Clipboard paste-path feature ─────────────────────────────────

const clipboardHotkeyID = 1

// clipboardSaveDir returns the directory to save clipboard images.
func clipboardSaveDir() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, "Pictures", "clipboard")
	os.MkdirAll(dir, 0755)
	return dir
}

// registerClipboardHotkey registers Ctrl+Shift+V as a global hotkey.
// Must be called from a thread with a message loop.
func registerClipboardHotkey(hwnd uintptr, log *slog.Logger) bool {
	ret, _, err := pRegisterHotKey.Call(hwnd, clipboardHotkeyID, modCtrl|modShift, vkV)
	if ret == 0 {
		log.Error("[CLIP] Failed to register Ctrl+Shift+V hotkey", "err", err)
		return false
	}
	log.Info("[CLIP] hotkey registered (Ctrl+Shift+V)")
	return true
}

// unregisterClipboardHotkey unregisters the hotkey.
func unregisterClipboardHotkey(hwnd uintptr) {
	pUnregisterHotKey.Call(hwnd, clipboardHotkeyID)
}

// pressShiftEnter sends a Shift+Enter virtual-key sequence via SendInput.
// Used to wrap the typed file path with newlines: Shift+Enter is a line
// break in chat apps (Claude Code, ChatGPT) and a regular newline in
// most editors, so it's safe in both contexts.
func pressShiftEnter() {
	const vkShift = 0x10
	const vkReturn = 0x0D
	var inp [4]kbInput
	inp[0] = kbInput{typ: inputKbd, vk: vkShift}
	inp[1] = kbInput{typ: inputKbd, vk: vkReturn}
	inp[2] = kbInput{typ: inputKbd, vk: vkReturn, flags: kfKeyup}
	inp[3] = kbInput{typ: inputKbd, vk: vkShift, flags: kfKeyup}
	pSendInput.Call(4, uintptr(unsafe.Pointer(&inp[0])), unsafe.Sizeof(inp[0]))
	time.Sleep(20 * time.Millisecond)
}

// handleClipboardHotkey is called when Ctrl+Shift+V is pressed.
// It reads the clipboard, extracts or saves the image, and types the file path
// into the active window using simulated keystrokes (same as STT typeText),
// surrounded by Shift+Enter newlines.
func handleClipboardHotkey(log *slog.Logger) {
	path, err := getClipboardImagePath(log)
	if err != nil {
		log.Warn("[CLIP] paste-path: no image", "err", err)
		return
	}
	log.Info("[CLIP] paste-path", "path", path)

	// Wait for Ctrl and Shift to be released before typing,
	// otherwise the typed characters combine with held modifiers
	// and trigger app shortcuts (e.g. Ctrl+Shift+C opens new terminal tab).
	waitForModifierRelease()
	// Use 0 for targetHwnd — clipboard hotkey doesn't need window restore
	pressShiftEnter()
	typeText(path, 0, log)
	pressShiftEnter()
}

// waitForModifierRelease polls until Ctrl and Shift are both released.
func waitForModifierRelease() {
	const vkControl = 0xA2 // VK_LCONTROL
	const vkRControl = 0xA3
	const vkLShift = 0xA0
	const vkRShift = 0xA1
	for {
		lc, _, _ := pGetAsyncKey.Call(vkControl)
		rc, _, _ := pGetAsyncKey.Call(vkRControl)
		ls, _, _ := pGetAsyncKey.Call(vkLShift)
		rs, _, _ := pGetAsyncKey.Call(vkRShift)
		if int16(lc) >= 0 && int16(rc) >= 0 && int16(ls) >= 0 && int16(rs) >= 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// getClipboardImagePath checks the clipboard and returns a file path.
// If the clipboard has files (CF_HDROP), returns the first file path.
// If the clipboard has a bitmap (CF_DIB), saves it as PNG and returns the path.
func getClipboardImagePath(log *slog.Logger) (string, error) {
	ret, _, _ := pOpenClipboard.Call(0)
	if ret == 0 {
		return "", fmt.Errorf("cannot open clipboard")
	}
	defer pCloseClipboard.Call()

	// Check for file drop first
	ret, _, _ = pIsClipboardFmtAvail.Call(cfHDROP)
	if ret != 0 {
		return getDropFilePath(log)
	}

	// Check for DIB bitmap (browser copies)
	ret, _, _ = pIsClipboardFmtAvail.Call(cfDIB)
	if ret != 0 {
		return saveDIBtoPNG(cfDIB, log)
	}

	// Check for DIBV5 bitmap (screenshots, modern apps)
	ret, _, _ = pIsClipboardFmtAvail.Call(cfDIBV5)
	if ret != 0 {
		return saveDIBtoPNG(cfDIBV5, log)
	}

	return "", fmt.Errorf("clipboard has no image or file")
}

// getDropFilePath extracts the first file path from CF_HDROP clipboard data.
func getDropFilePath(log *slog.Logger) (string, error) {
	hDrop, _, _ := pGetClipboardData.Call(cfHDROP)
	if hDrop == 0 {
		return "", fmt.Errorf("cannot get CF_HDROP data")
	}

	// Get the length of the first file name
	nameLen, _, _ := pDragQueryFileW.Call(hDrop, 0, 0, 0)
	if nameLen == 0 {
		return "", fmt.Errorf("empty file drop")
	}

	buf := make([]uint16, nameLen+1)
	pDragQueryFileW.Call(hDrop, 0, uintptr(unsafe.Pointer(&buf[0])), nameLen+1)
	path := windows.UTF16ToString(buf)

	log.Info("[CLIP] file drop", "path", path)
	return path, nil
}

// saveDIBtoPNG extracts CF_DIB or CF_DIBV5 bitmap data, converts to PNG, and saves to disk.
func saveDIBtoPNG(format uintptr, log *slog.Logger) (string, error) {
	hMem, _, _ := pGetClipboardData.Call(format)
	if hMem == 0 {
		return "", fmt.Errorf("cannot get CF_DIB data")
	}

	ptr, _, _ := pGlobalLock.Call(hMem)
	if ptr == 0 {
		return "", fmt.Errorf("cannot lock DIB memory")
	}
	defer pGlobalUnlock.Call(hMem)

	// Read BITMAPINFOHEADER
	hdr := (*bitmapInfoHeader)(unsafe.Pointer(ptr))

	// BI_RGB=0, BI_BITFIELDS=3 are the common formats
	if hdr.Compression != 0 && hdr.Compression != 3 {
		return "", fmt.Errorf("unsupported DIB compression: %d", hdr.Compression)
	}

	width := int(hdr.Width)
	height := int(hdr.Height)
	if err := validateDIBBounds(hdr.Width, hdr.Height); err != nil {
		return "", err
	}
	bottomUp := true
	if height < 0 {
		height = -height
		bottomUp = false
	}
	bitCount := int(hdr.BitCount)

	if bitCount != 24 && bitCount != 32 {
		return "", fmt.Errorf("unsupported bit depth: %d", bitCount)
	}

	// Calculate pixel data offset (after header + color table)
	pixelOffset := uintptr(hdr.Size)
	if hdr.Compression == 3 {
		// BI_BITFIELDS: 3 DWORD masks follow the header (if not already included in header size)
		if hdr.Size == 40 {
			pixelOffset += 12 // 3 × 4 bytes for R, G, B masks
		}
	}
	if hdr.ClrUsed > 0 {
		pixelOffset += uintptr(hdr.ClrUsed) * 4
	}

	// Row stride is padded to 4 bytes
	rowStride := ((width*bitCount + 31) / 32) * 4
	totalPixelBytes := rowStride * height

	// Copy pixel data to Go slice (clipboard memory can be freed anytime)
	pixelData := make([]byte, totalPixelBytes)
	src := unsafe.Slice((*byte)(unsafe.Pointer(ptr+pixelOffset)), totalPixelBytes)
	copy(pixelData, src)

	// Build image
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		srcY := y
		if bottomUp {
			srcY = height - 1 - y
		}
		rowStart := srcY * rowStride
		for x := 0; x < width; x++ {
			offset := rowStart + x*(bitCount/8)
			b := pixelData[offset]
			g := pixelData[offset+1]
			r := pixelData[offset+2]
			a := byte(255)
			if bitCount == 32 {
				a = pixelData[offset+3]
				if a == 0 {
					a = 255 // many DIBs have alpha=0 but mean opaque
				}
			}
			img.SetRGBA(x, y, color.RGBA{R: r, G: g, B: b, A: a})
		}
	}

	// Save as PNG
	ts := time.Now().Format("20060102_150405")
	filename := fmt.Sprintf("clipboard_%s.png", ts)
	savePath := filepath.Join(clipboardSaveDir(), filename)

	f, err := os.Create(savePath)
	if err != nil {
		return "", fmt.Errorf("cannot create file: %w", err)
	}
	defer f.Close()

	if err := png.Encode(f, img); err != nil {
		return "", fmt.Errorf("PNG encode failed: %w", err)
	}

	log.Info("[CLIP] saved image", "path", savePath, "size", fmt.Sprintf("%dx%d", width, height))
	return savePath, nil
}

// ── Hotkey message loop ──────────────────────────────────────────

const pmRemove = 0x0001

type msgStruct struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      [2]int32
}

// runClipboardHotkey runs in a dedicated goroutine with a locked OS thread.
// It registers Ctrl+Shift+V and processes WM_HOTKEY messages.
func runClipboardHotkey(ctx context.Context, log *slog.Logger) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if !registerClipboardHotkey(0, log) {
		return
	}
	defer unregisterClipboardHotkey(0)

	var msg msgStruct
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			for {
				ret, _, _ := pPeekMessageW.Call(
					uintptr(unsafe.Pointer(&msg)),
					0, 0, 0, pmRemove,
				)
				if ret == 0 {
					break
				}
				if msg.Message == wmHotkey && msg.WParam == clipboardHotkeyID {
					handleClipboardHotkey(log)
				}
			}
		}
	}
}
