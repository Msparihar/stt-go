//go:build windows

package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ── Clipboard constants ──────────────────────────────────────────

const (
	cfBitmap      = 2
	cfDIB         = 8
	cfDIBV5       = 17
	cfHDROP       = 15
	cfUnicodeText = 13
	gmemMoveable  = 0x0002

	modCtrl  = 0x0002
	modShift = 0x0004
	vkB      = 0x42

	wmHotkey = 0x0312
)

// ── Clipboard DLL procs ──────────────────────────────────────────

var (
	kernel32 = windows.NewLazyDLL("kernel32.dll")
	shell32  = windows.NewLazyDLL("shell32.dll")

	pOpenClipboard       = user32.NewProc("OpenClipboard")
	pCloseClipboard      = user32.NewProc("CloseClipboard")
	pEmptyClipboard      = user32.NewProc("EmptyClipboard")
	pGetClipboardData    = user32.NewProc("GetClipboardData")
	pSetClipboardData    = user32.NewProc("SetClipboardData")
	pIsClipboardFmtAvail = user32.NewProc("IsClipboardFormatAvailable")
	pRegisterHotKey      = user32.NewProc("RegisterHotKey")
	pUnregisterHotKey    = user32.NewProc("UnregisterHotKey")

	pGlobalAlloc = kernel32.NewProc("GlobalAlloc")
	pGlobalLock  = kernel32.NewProc("GlobalLock")
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

// ── Clipboard paste-path feature ─────────────────────────────────

const clipboardHotkeyID = 1

// clipboardSaveDir returns the directory to save clipboard images.
func clipboardSaveDir() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, "Pictures", "clipboard")
	os.MkdirAll(dir, 0755)
	return dir
}

// registerClipboardHotkey registers Ctrl+Shift+B as a global hotkey.
// Must be called from a thread with a message loop.
func registerClipboardHotkey(hwnd uintptr, log *slog.Logger) bool {
	ret, _, err := pRegisterHotKey.Call(hwnd, clipboardHotkeyID, modCtrl|modShift, vkB)
	if ret == 0 {
		log.Error("Failed to register Ctrl+Shift+B hotkey", "err", err)
		return false
	}
	log.Info("Clipboard paste-path hotkey registered (Ctrl+Shift+B)")
	return true
}

// unregisterClipboardHotkey unregisters the hotkey.
func unregisterClipboardHotkey(hwnd uintptr) {
	pUnregisterHotKey.Call(hwnd, clipboardHotkeyID)
}

// handleClipboardHotkey is called when Ctrl+Shift+B is pressed.
// It reads the clipboard, extracts or saves the image, and types the file path
// into the active window using simulated keystrokes (same as STT typeText).
func handleClipboardHotkey(log *slog.Logger) {
	path, err := getClipboardImagePath(log)
	if err != nil {
		log.Warn("Clipboard paste-path: no image", "err", err)
		return
	}
	log.Info("Clipboard paste-path", "path", path)

	// Wait for Ctrl and Shift to be released before typing,
	// otherwise the typed characters combine with held modifiers
	// and trigger app shortcuts (e.g. Ctrl+Shift+C opens new terminal tab).
	waitForModifierRelease()
	// Use 0 for targetHwnd — clipboard hotkey doesn't need window restore
	typeText(path, 0, log)
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

	log.Info("Clipboard file drop", "path", path)
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

	log.Info("Saved clipboard image", "path", savePath, "size", fmt.Sprintf("%dx%d", width, height))
	return savePath, nil
}

// setClipboardText sets the clipboard content to a text string.
// Clipboard must NOT be open when calling this.
func setClipboardText(text string, log *slog.Logger) error {
	ret, _, _ := pOpenClipboard.Call(0)
	if ret == 0 {
		return fmt.Errorf("cannot open clipboard")
	}
	defer pCloseClipboard.Call()

	pEmptyClipboard.Call()

	utf16 := windows.StringToUTF16(text)
	size := len(utf16) * 2

	hMem, _, _ := pGlobalAlloc.Call(gmemMoveable, uintptr(size))
	if hMem == 0 {
		return fmt.Errorf("GlobalAlloc failed")
	}

	ptr, _, _ := pGlobalLock.Call(hMem)
	if ptr == 0 {
		return fmt.Errorf("GlobalLock failed")
	}

	dst := unsafe.Slice((*byte)(unsafe.Pointer(ptr)), size)
	for i, v := range utf16 {
		binary.LittleEndian.PutUint16(dst[i*2:], v)
	}
	pGlobalUnlock.Call(hMem)

	ret, _, _ = pSetClipboardData.Call(cfUnicodeText, hMem)
	if ret == 0 {
		return fmt.Errorf("SetClipboardData failed")
	}

	log.Info("Set clipboard text", "text", truncate(text, 80))
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// isImageFile checks if a path looks like an image file.
func isImageFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".bmp", ".webp", ".ico", ".tiff", ".svg":
		return true
	}
	return false
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
// It registers Ctrl+Shift+B and processes WM_HOTKEY messages.
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
