//go:build windows

package main

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ── Waveform overlay constants ─────────────────────────────────────

// Overlay modes
type overlayMode int

const (
	modeRecording    overlayMode = iota
	modeTranscribing
)

const (
	overlayW       = 280
	overlayH       = 50
	overlayPadding = 10
	barWidth       = 2.5
	barGap         = 1.0
	barCount       = 38
	barMinH        = 2.0
	barJitter      = 0.20
	decayRate      = 0.92

	// Transcribing overlay dimensions
	transcribeW       = 50
	transcribeH       = 50
	catPixel          = 3.0 // size of each pixel in the sprite
	catCols           = 11
	catRows           = 11
	blinkInterval     = 60  // blink every ~60 ticks (~1.8s at 30ms)
	blinkDuration     = 4   // blink lasts ~4 ticks (~120ms)
	transcribeTimeout = 30 * time.Second

	// Win32 window constants
	wsExToolWindow  = 0x00000080
	wsExTopmost     = 0x00000008
	wsExLayered     = 0x00080000
	wsExTransparent = 0x00000020
	wsExNoActivate  = 0x08000000
	wsPopup         = 0x80000000
	swShow          = 5
	swHide          = 0
	wmPaint         = 0x000F
	wmUser          = 0x0400
	wmRedrawWave    = wmUser + 1
	wmShowOverlay   = wmUser + 2
	wmHideOverlay   = wmUser + 3
	wmShowTranscribing = wmUser + 4
	lwaAlpha        = 0x02
	spiGetWorkArea  = 0x0030
	swpNoZOrder     = 0x0004
	swpNoActivate   = 0x0010

	// Direct2D constants
	d2d1FactoryTypeSingleThreaded = 0
	dxgiFormatB8G8R8A8Unorm       = 87
	d2d1AlphaModeUnknown          = 0
)

// Direct2D COM GUIDs
var iidID2D1Factory = [16]byte{
	0x47, 0x22, 0x15, 0x06, 0x50, 0x6f, 0x5a, 0x46,
	0x92, 0x45, 0x11, 0x8b, 0xfd, 0x3b, 0x60, 0x07,
}

// ── Direct2D structs ───────────────────────────────────────────────

type d2dPixelFormat struct {
	Format    uint32
	AlphaMode uint32
}

type d2dRenderTargetProps struct {
	Type        uint32
	PixelFormat d2dPixelFormat
	DpiX        float32
	DpiY        float32
	Usage       uint32
	MinLevel    uint32
}

type d2dSizeU struct {
	Width  uint32
	Height uint32
}

type d2dHwndRenderTargetProps struct {
	Hwnd           uintptr
	PixelSize      d2dSizeU
	PresentOptions uint32
}

type d2dColorF struct {
	R, G, B, A float32
}

type d2dPointF struct {
	X, Y float32
}

type d2dRoundedRect struct {
	Left, Top, Right, Bottom float32
	RadiusX, RadiusY         float32
}

// ── COM helpers ────────────────────────────────────────────────────

func comCall(obj uintptr, vtableIdx int, args ...uintptr) uintptr {
	vtable := *(*uintptr)(unsafe.Pointer(obj))
	method := *(*uintptr)(unsafe.Pointer(vtable + uintptr(vtableIdx)*unsafe.Sizeof(uintptr(0))))
	allArgs := append([]uintptr{obj}, args...)
	r1, _, _ := syscall.SyscallN(method, allArgs...)
	return r1
}

func packF32x2(a, b float32) uintptr {
	lo := *(*uint32)(unsafe.Pointer(&a))
	hi := *(*uint32)(unsafe.Pointer(&b))
	return uintptr(lo) | uintptr(hi)<<32
}

func f32bits(v float32) uintptr {
	return uintptr(*(*uint32)(unsafe.Pointer(&v)))
}

// ── Win32 structs for window ───────────────────────────────────────

type wndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  uintptr
	LpszClassName uintptr
	HIconSm       uintptr
}

type point struct{ X, Y int32 }
type rect struct{ Left, Top, Right, Bottom int32 }
type msg struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}
type paintStruct struct {
	HDC         uintptr
	FErase      int32
	RcPaint     rect
	FRestore    int32
	FIncUpdate  int32
	RgbReserved [32]byte
}

// ── Waveform overlay ───────────────────────────────────────────────

type waveOverlay struct {
	hwnd         uintptr
	visible      bool
	mode         overlayMode
	log          *slog.Logger
	factory      uintptr
	renderTarget uintptr
	stopDecay    chan struct{}
	stopTranscribe chan struct{}
	hideOnce     sync.Once
	animTick     int

	sampleMu sync.Mutex
	bars     [barCount]float32
}

var globalOverlay *waveOverlay

func waveWndProc(hwnd, umsg, wparam, lparam uintptr) uintptr {
	switch umsg {
	case wmPaint:
		if globalOverlay != nil {
			globalOverlay.paint()
		}
		var ps paintStruct
		pBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		pEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0
	case wmShowOverlay:
		pShowWindow.Call(hwnd, swShow)
		return 0
	case wmHideOverlay:
		if globalOverlay != nil {
			globalOverlay.resizeWindow(overlayW, overlayH)
		}
		pShowWindow.Call(hwnd, swHide)
		return 0
	case wmShowTranscribing:
		if globalOverlay != nil {
			globalOverlay.resizeWindow(transcribeW, transcribeH)
		}
		return 0
	case wmRedrawWave:
		pInvalidateRect.Call(hwnd, 0, 0)
		return 0
	}
	ret, _, _ := pDefWindowProcW.Call(hwnd, umsg, wparam, lparam)
	return ret
}

func newWaveOverlay(log *slog.Logger) *waveOverlay {
	w := &waveOverlay{log: log}
	globalOverlay = w
	go w.runMessageLoop()
	for i := 0; i < 200; i++ {
		time.Sleep(10 * time.Millisecond)
		if w.hwnd != 0 {
			break
		}
	}
	return w
}

func (w *waveOverlay) runMessageLoop() {
	runtime.LockOSThread()

	hr, _, _ := pD2D1CreateFactory.Call(
		d2d1FactoryTypeSingleThreaded,
		uintptr(unsafe.Pointer(&iidID2D1Factory)),
		0,
		uintptr(unsafe.Pointer(&w.factory)),
	)
	if hr != 0 {
		w.log.Error("D2D1CreateFactory failed", "hr", fmt.Sprintf("0x%x", hr))
		return
	}

	className, _ := windows.UTF16PtrFromString("STTWaveD2D")
	cb := windows.NewCallback(waveWndProc)
	wcx := wndClassExW{
		Style:         3,
		LpfnWndProc:   cb,
		LpszClassName: uintptr(unsafe.Pointer(className)),
	}
	wcx.CbSize = uint32(unsafe.Sizeof(wcx))
	pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wcx)))

	var workArea rect
	pSystemParamInfo.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&workArea)), 0)
	screenW := int(workArea.Right - workArea.Left)
	posX := workArea.Left + int32(screenW/2-overlayW/2)
	posY := workArea.Bottom - int32(overlayH+12)

	exStyle := uintptr(wsExToolWindow | wsExTopmost | wsExLayered | wsExNoActivate)
	windowName, _ := windows.UTF16PtrFromString("STT Wave")
	hwnd, _, _ := pCreateWindowExW.Call(
		exStyle, uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(windowName)), uintptr(wsPopup),
		uintptr(posX), uintptr(posY), uintptr(overlayW), uintptr(overlayH),
		0, 0, 0, 0,
	)
	if hwnd == 0 {
		w.log.Error("Failed to create overlay window")
		return
	}

	pSetLayeredWndAttr.Call(hwnd, 0, 230, lwaAlpha)

	rgn, _, _ := pCreateRoundRectRgn.Call(0, 0, overlayW, overlayH, overlayH, overlayH)
	pSetWindowRgn.Call(hwnd, rgn, 1)

	w.hwnd = hwnd

	rtProps := d2dRenderTargetProps{
		PixelFormat: d2dPixelFormat{Format: dxgiFormatB8G8R8A8Unorm, AlphaMode: d2d1AlphaModeUnknown},
	}
	hwndProps := d2dHwndRenderTargetProps{
		Hwnd:      hwnd,
		PixelSize: d2dSizeU{Width: overlayW, Height: overlayH},
	}
	hr = comCall(w.factory, 14,
		uintptr(unsafe.Pointer(&rtProps)),
		uintptr(unsafe.Pointer(&hwndProps)),
		uintptr(unsafe.Pointer(&w.renderTarget)),
	)
	if hr != 0 {
		w.log.Error("CreateHwndRenderTarget failed", "hr", fmt.Sprintf("0x%x", hr))
		return
	}

	w.log.Info("Waveform overlay created (Direct2D)")

	var m msg
	for {
		ret, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if ret == 0 || int32(ret) == -1 {
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func (w *waveOverlay) paint() {
	if w.renderTarget == 0 {
		return
	}
	switch w.mode {
	case modeTranscribing:
		w.paintTranscribing()
	default:
		w.paintWaveform()
	}
}

func (w *waveOverlay) paintWaveform() {
	rt := w.renderTarget

	comCall(rt, 48)

	bgColor := d2dColorF{R: 0.10, G: 0.10, B: 0.12, A: 1.0}
	comCall(rt, 47, uintptr(unsafe.Pointer(&bgColor)))

	var bgBrush uintptr
	pillBg := d2dColorF{R: 0.10, G: 0.10, B: 0.12, A: 1.0}
	comCall(rt, 8, uintptr(unsafe.Pointer(&pillBg)), 0, uintptr(unsafe.Pointer(&bgBrush)))
	if bgBrush != 0 {
		pill := d2dRoundedRect{
			Left: 0, Top: 0, Right: float32(overlayW), Bottom: float32(overlayH),
			RadiusX: float32(overlayH) / 2, RadiusY: float32(overlayH) / 2,
		}
		comCall(rt, 19, uintptr(unsafe.Pointer(&pill)), bgBrush)
		comCall(bgBrush, 2)
	}

	w.sampleMu.Lock()
	var rawBars [barCount]float32
	copy(rawBars[:], w.bars[:])
	w.sampleMu.Unlock()

	midY := float32(overlayH) / 2
	maxAmp := midY - float32(overlayPadding)
	centerX := float32(overlayW) / 2
	step := float32(barWidth + barGap)

	for i := range barCount {
		amp := rawBars[i]
		halfH := amp * maxAmp
		if halfH < barMinH {
			halfH = barMinH
		}

		edgeFade := float32(1.0)
		if i > barCount-8 {
			edgeFade = float32(barCount-i) / 8.0
		}

		var barBrush uintptr
		barColor := d2dColorF{R: 0.9, G: 0.9, B: 0.92, A: 0.85 * edgeFade}
		comCall(rt, 8, uintptr(unsafe.Pointer(&barColor)), 0, uintptr(unsafe.Pointer(&barBrush)))
		if barBrush == 0 {
			continue
		}

		rx := centerX + float32(i)*step + barGap/2
		comCall(rt, 15,
			packF32x2(rx, midY-halfH),
			packF32x2(rx, midY+halfH),
			barBrush,
			f32bits(barWidth),
			0,
		)

		lx := centerX - float32(i)*step - barGap/2
		comCall(rt, 15,
			packF32x2(lx, midY-halfH),
			packF32x2(lx, midY+halfH),
			barBrush,
			f32bits(barWidth),
			0,
		)

		comCall(barBrush, 2)
	}

	comCall(rt, 49, 0, 0)
}

// Cat sprite pixel types
const (
	_ = iota // 0 = transparent
	W        // 1 = white (body)
	E        // 2 = eye (green)
	N        // 3 = nose (pink)
	B        // 4 = blink (eye closed = white line)
)

// Cat sprite: 11x11 grid
// Ears on top, face, body, legs, tail wags left/right
var catFrameTailRight = [catRows][catCols]byte{
	// row 0:  ears
	{0, W, 0, 0, 0, 0, 0, 0, 0, W, 0},
	// row 1:  ear tips into head
	{W, W, W, 0, 0, 0, 0, 0, W, W, W},
	// row 2:  head top
	{0, W, W, W, W, W, W, W, W, W, 0},
	// row 3:  eyes
	{0, W, E, W, W, W, W, E, W, W, 0},
	// row 4:  nose
	{0, W, W, W, W, N, W, W, W, W, 0},
	// row 5:  mouth
	{0, W, W, W, W, W, W, W, W, W, 0},
	// row 6:  body top
	{0, 0, W, W, W, W, W, W, W, 0, 0},
	// row 7:  body
	{0, 0, W, W, W, W, W, W, W, 0, 0},
	// row 8:  body bottom
	{0, 0, W, W, W, W, W, W, W, 0, 0},
	// row 9:  legs
	{0, 0, W, W, 0, 0, 0, W, W, 0, 0},
	// row 10: tail right
	{0, 0, 0, 0, 0, 0, 0, 0, W, W, W},
}

var catFrameTailLeft = [catRows][catCols]byte{
	{0, W, 0, 0, 0, 0, 0, 0, 0, W, 0},
	{W, W, W, 0, 0, 0, 0, 0, W, W, W},
	{0, W, W, W, W, W, W, W, W, W, 0},
	{0, W, E, W, W, W, W, E, W, W, 0},
	{0, W, W, W, W, N, W, W, W, W, 0},
	{0, W, W, W, W, W, W, W, W, W, 0},
	{0, 0, W, W, W, W, W, W, W, 0, 0},
	{0, 0, W, W, W, W, W, W, W, 0, 0},
	{0, 0, W, W, W, W, W, W, W, 0, 0},
	{0, 0, W, W, 0, 0, 0, W, W, 0, 0},
	{W, W, W, 0, 0, 0, 0, 0, 0, 0, 0},
}

var catColors = map[byte]d2dColorF{
	W: {R: 0.90, G: 0.90, B: 0.92, A: 1.0}, // white body
	E: {R: 0.30, G: 0.85, B: 0.45, A: 1.0}, // green eyes
	N: {R: 1.00, G: 0.60, B: 0.70, A: 1.0}, // pink nose
	B: {R: 0.90, G: 0.90, B: 0.92, A: 1.0}, // blink (same as body)
}

func (w *waveOverlay) paintTranscribing() {
	rt := w.renderTarget

	comCall(rt, 48)

	bgColor := d2dColorF{R: 0.10, G: 0.10, B: 0.12, A: 1.0}
	comCall(rt, 47, uintptr(unsafe.Pointer(&bgColor)))

	// Draw rounded square background
	var bgBrush uintptr
	pillBg := d2dColorF{R: 0.10, G: 0.10, B: 0.12, A: 1.0}
	comCall(rt, 8, uintptr(unsafe.Pointer(&pillBg)), 0, uintptr(unsafe.Pointer(&bgBrush)))
	if bgBrush != 0 {
		pill := d2dRoundedRect{
			Left: 0, Top: 0, Right: float32(transcribeW), Bottom: float32(transcribeH),
			RadiusX: 10, RadiusY: 10,
		}
		comCall(rt, 19, uintptr(unsafe.Pointer(&pill)), bgBrush)
		comCall(bgBrush, 2)
	}

	tick := w.animTick

	// Pick frame: tail wags every ~15 ticks (~450ms)
	frame := &catFrameTailRight
	if (tick/15)%2 == 1 {
		frame = &catFrameTailLeft
	}

	// Blinking: eyes become horizontal line
	blinking := (tick%blinkInterval) < blinkDuration

	// Center the sprite in the pill
	spriteW := float32(catCols) * catPixel
	spriteH := float32(catRows) * catPixel
	offX := (float32(transcribeW) - spriteW) / 2
	offY := (float32(transcribeH) - spriteH) / 2

	// Create brushes for each color (reuse across pixels)
	brushes := make(map[byte]uintptr)
	for id, clr := range catColors {
		var br uintptr
		c := clr
		comCall(rt, 8, uintptr(unsafe.Pointer(&c)), 0, uintptr(unsafe.Pointer(&br)))
		if br != 0 {
			brushes[id] = br
		}
	}

	for row := 0; row < catRows; row++ {
		for col := 0; col < catCols; col++ {
			px := frame[row][col]
			if px == 0 {
				continue
			}

			// Handle blink: replace eye pixels with a horizontal line
			if blinking && px == E {
				px = B
				// Draw a thin horizontal line instead of a filled square
				br := brushes[B]
				if br != 0 {
					ly := offY + float32(row)*catPixel + catPixel/2
					lx1 := offX + float32(col)*catPixel
					lx2 := lx1 + catPixel
					comCall(rt, 15,
						packF32x2(lx1, ly),
						packF32x2(lx2, ly),
						br,
						f32bits(1.0),
						0,
					)
				}
				continue
			}

			br := brushes[px]
			if br == 0 {
				continue
			}

			// FillRectangle (vtable 17): fill a pixel-sized rect
			x := offX + float32(col)*catPixel
			y := offY + float32(row)*catPixel
			r := d2dColorF{} // reuse as rect
			_ = r
			rect := [4]float32{x, y, x + catPixel, y + catPixel}
			comCall(rt, 17, uintptr(unsafe.Pointer(&rect)), br)
		}
	}

	// Release brushes
	for _, br := range brushes {
		comCall(br, 2)
	}

	comCall(rt, 49, 0, 0)
}

func (w *waveOverlay) pushAudio(pcm []byte) {
	nSamples := len(pcm) / 2
	if nSamples == 0 {
		return
	}

	subBars := 4
	subSize := nSamples / subBars
	if subSize < 1 {
		subSize = 1
	}

	var newAmps [8]float32
	if subBars > 8 {
		subBars = 8
	}
	for i := 0; i < subBars; i++ {
		start := i * subSize
		end := start + subSize
		if end > nSamples {
			end = nSamples
		}
		var sum float64
		for j := start; j < end; j++ {
			s := int16(binary.LittleEndian.Uint16(pcm[j*2:]))
			sum += float64(s) * float64(s)
		}
		rms := math.Sqrt(sum / float64(end-start))
		amp := float32(rms / 2000.0)
		if amp > 1 {
			amp = 1
		}
		amp = float32(math.Sqrt(float64(amp)))
		jitter := 1.0 + barJitter*(rand.Float32()*2-1)
		amp *= jitter
		if amp > 1 {
			amp = 1
		}
		newAmps[i] = amp
	}

	w.sampleMu.Lock()
	copy(w.bars[subBars:], w.bars[:barCount-subBars])
	for i := 0; i < subBars; i++ {
		w.bars[i] = newAmps[subBars-1-i]
	}
	w.sampleMu.Unlock()

	if w.hwnd != 0 {
		pPostMessageW.Call(w.hwnd, wmRedrawWave, 0, 0)
	}
}

func (w *waveOverlay) show() {
	if w.hwnd != 0 && !w.visible {
		w.visible = true
		w.mode = modeRecording
		w.hideOnce = sync.Once{}
		w.sampleMu.Lock()
		w.bars = [barCount]float32{}
		w.sampleMu.Unlock()

		w.stopDecay = make(chan struct{})
		go func() {
			defer func() {
				if p := recover(); p != nil {
					w.log.Error("overlay decay goroutine: panic recovered", "panic", fmt.Sprintf("%v", p))
				}
			}()
			tick := time.NewTicker(30 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-w.stopDecay:
					return
				case <-tick.C:
					w.sampleMu.Lock()
					for i := range barCount {
						w.bars[i] *= decayRate
					}
					w.sampleMu.Unlock()
					if w.hwnd != 0 {
						pPostMessageW.Call(w.hwnd, wmRedrawWave, 0, 0)
					}
				}
			}
		}()

		pPostMessageW.Call(w.hwnd, wmShowOverlay, 0, 0)
	}
}

func (w *waveOverlay) hide() {
	if w.hwnd == 0 {
		return
	}
	w.hideOnce.Do(func() {
		w.visible = false
		if w.stopDecay != nil {
			close(w.stopDecay)
			w.stopDecay = nil
		}
		if w.stopTranscribe != nil {
			close(w.stopTranscribe)
			w.stopTranscribe = nil
		}
		w.mode = modeRecording
		pPostMessageW.Call(w.hwnd, wmHideOverlay, 0, 0)
	})
}

func (w *waveOverlay) showTranscribing() {
	if w.hwnd == 0 {
		return
	}
	// Stop the waveform decay animation
	if w.stopDecay != nil {
		close(w.stopDecay)
		w.stopDecay = nil
	}
	w.mode = modeTranscribing
	w.animTick = 0
	w.hideOnce = sync.Once{}

	if !w.visible {
		w.visible = true
		pPostMessageW.Call(w.hwnd, wmShowOverlay, 0, 0)
	}

	// Resize to smaller transcribing pill (handled on window thread)
	pPostMessageW.Call(w.hwnd, wmShowTranscribing, 0, 0)

	// Start animation ticker
	w.stopTranscribe = make(chan struct{})
	go func() {
		defer func() {
			if p := recover(); p != nil {
				w.log.Error("overlay transcribe goroutine: panic recovered", "panic", fmt.Sprintf("%v", p))
			}
		}()
		tick := time.NewTicker(30 * time.Millisecond)
		defer tick.Stop()
		timeout := time.NewTimer(transcribeTimeout)
		defer timeout.Stop()
		for {
			select {
			case <-w.stopTranscribe:
				return
			case <-timeout.C:
				w.log.Warn("Transcription overlay timeout, auto-hiding")
				w.hide()
				return
			case <-tick.C:
				w.animTick++
				if w.hwnd != 0 {
					pPostMessageW.Call(w.hwnd, wmRedrawWave, 0, 0)
				}
			}
		}
	}()
}

func (w *waveOverlay) resizeWindow(width, height int) {
	var workArea rect
	pSystemParamInfo.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&workArea)), 0)
	screenW := int(workArea.Right - workArea.Left)
	posX := int(workArea.Left) + screenW/2 - width/2
	posY := int(workArea.Bottom) - height - 12

	pMoveWindow.Call(w.hwnd, uintptr(posX), uintptr(posY), uintptr(width), uintptr(height), 1)

	// Update rounded region
	rgn, _, _ := pCreateRoundRectRgn.Call(0, 0, uintptr(width), uintptr(height), uintptr(height), uintptr(height))
	pSetWindowRgn.Call(w.hwnd, rgn, 1)

	// Resize D2D render target (ID2D1HwndRenderTarget::Resize = vtable index 58)
	if w.renderTarget != 0 {
		size := d2dSizeU{Width: uint32(width), Height: uint32(height)}
		comCall(w.renderTarget, 58, uintptr(unsafe.Pointer(&size)))
	}
}
