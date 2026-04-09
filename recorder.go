//go:build windows

package main

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ── Windows audio structs ──────────────────────────────────────────

type waveFormatEx struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	CbSize         uint16
}

type waveHdr struct {
	LpData   uintptr
	BufLen   uint32
	Recorded uint32
	User     uintptr
	Flags    uint32
	Loops    uint32
	Next     uintptr
	Reserved uintptr
}

type waveInCapsW struct {
	ManufacturerID uint16
	ProductID      uint16
	DriverVersion  uint32
	ProductName    [32]uint16 // MAXPNAMELEN = 32
	Formats        uint32
	Channels       uint16
	Reserved       uint16
}

// ── Mic enumeration ────────────────────────────────────────────────

type micDevice struct {
	ID   uintptr // device index for waveInOpen
	Name string
}

func listMics() []micDevice {
	numDevs, _, _ := pWaveInGetNumDevs.Call()
	var mics []micDevice
	for i := uintptr(0); i < numDevs; i++ {
		var caps waveInCapsW
		ret, _, _ := pWaveInGetDevCapsW.Call(i, uintptr(unsafe.Pointer(&caps)), unsafe.Sizeof(caps))
		if ret == 0 {
			name := windows.UTF16ToString(caps.ProductName[:])
			mics = append(mics, micDevice{ID: i, Name: name})
		}
	}
	return mics
}

func getDefaultMicName() string {
	numDevs, _, _ := pWaveInGetNumDevs.Call()
	if numDevs == 0 {
		return "No microphone found"
	}
	var caps waveInCapsW
	ret, _, _ := pWaveInGetDevCapsW.Call(0, uintptr(unsafe.Pointer(&caps)), unsafe.Sizeof(caps))
	if ret != 0 {
		return "Unknown microphone"
	}
	return windows.UTF16ToString(caps.ProductName[:])
}

// ── Audio recorder ─────────────────────────────────────────────────

type recorder struct {
	hwi      uintptr
	event    windows.Handle
	hdrs     [numBufs]waveHdr
	bufs     [numBufs][]byte
	mu       sync.Mutex
	running  bool
	done     chan struct{}
	deviceID uintptr

	allData   []byte
	byteCount int
	onChunk   func([]byte)
	log       *slog.Logger
}

func newRecorder(log *slog.Logger) *recorder {
	return &recorder{log: log, deviceID: waveMapper}
}

func (r *recorder) setDeviceID(id uintptr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deviceID = id
}

func (r *recorder) start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return nil
	}

	ev, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return fmt.Errorf("CreateEvent: %w", err)
	}
	r.event = ev
	r.allData = nil
	r.byteCount = 0

	wfx := waveFormatEx{
		FormatTag:      wavFmtPCM,
		Channels:       audioCh,
		SamplesPerSec:  sampleRate,
		AvgBytesPerSec: avgBytesPerSec,
		BlockAlign:     blockAlign,
		BitsPerSample:  bitsPerSample,
	}

	ret, _, _ := pWaveInOpen.Call(
		uintptr(unsafe.Pointer(&r.hwi)),
		r.deviceID, uintptr(unsafe.Pointer(&wfx)),
		uintptr(ev), 0, cbEvent,
	)
	if ret != 0 {
		windows.CloseHandle(ev)
		return fmt.Errorf("waveInOpen MMRESULT %d", ret)
	}

	for i := range numBufs {
		r.bufs[i] = make([]byte, bufSize)
		r.hdrs[i] = waveHdr{
			LpData: uintptr(unsafe.Pointer(&r.bufs[i][0])),
			BufLen: uint32(bufSize),
		}
		pWaveInPrepHdr.Call(r.hwi, uintptr(unsafe.Pointer(&r.hdrs[i])), unsafe.Sizeof(r.hdrs[i]))
		pWaveInAddBuf.Call(r.hwi, uintptr(unsafe.Pointer(&r.hdrs[i])), unsafe.Sizeof(r.hdrs[i]))
	}

	ret, _, _ = pWaveInStart.Call(r.hwi)
	if ret != 0 {
		r.cleanup()
		return fmt.Errorf("waveInStart MMRESULT %d", ret)
	}
	r.running = true
	r.done = make(chan struct{})
	go r.loop()
	r.log.Info("Recording started")
	return nil
}

func (r *recorder) loop() {
	defer close(r.done)
	defer func() {
		if p := recover(); p != nil {
			r.log.Error("recorder.loop: panic recovered", "panic", fmt.Sprintf("%v", p))
		}
	}()
	for {
		r.mu.Lock()
		running := r.running
		r.mu.Unlock()

		if !running {
			r.processBufs()
			return
		}

		windows.WaitForSingleObject(r.event, 100)
		r.processBufs()
	}
}

func (r *recorder) processBufs() {
	for i := range numBufs {
		if r.hdrs[i].Flags&whdrDone != 0 && r.hdrs[i].Recorded > 0 {
			n := r.hdrs[i].Recorded
			data := make([]byte, n)
			copy(data, unsafe.Slice((*byte)(unsafe.Pointer(r.hdrs[i].LpData)), n))

			r.byteCount += len(data)
			if r.onChunk != nil {
				r.onChunk(data)
			} else {
				r.allData = append(r.allData, data...)
			}

			r.mu.Lock()
			if r.running {
				r.hdrs[i].Recorded = 0
				r.hdrs[i].Flags &^= whdrDone
				pWaveInAddBuf.Call(r.hwi, uintptr(unsafe.Pointer(&r.hdrs[i])), unsafe.Sizeof(r.hdrs[i]))
			}
			r.mu.Unlock()
		}
	}
}

func (r *recorder) stop() (pcm []byte, total int) {
	// Drain: keep recording for a short window after key release
	// to capture trailing speech that may still be in the mic buffer
	time.Sleep(200 * time.Millisecond)

	r.mu.Lock()
	r.running = false
	r.mu.Unlock()

	pWaveInStop.Call(r.hwi)
	pWaveInReset.Call(r.hwi)
	windows.SetEvent(r.event)

	<-r.done
	r.cleanup()

	r.log.Info("Recording stopped", "bytes", r.byteCount,
		"duration", fmt.Sprintf("%.1fs", float64(r.byteCount)/float64(avgBytesPerSec)))
	return r.allData, r.byteCount
}

func (r *recorder) cleanup() {
	r.log.Info("recorder.cleanup: starting", "hwi", fmt.Sprintf("0x%X", r.hwi))

	if r.hwi == 0 {
		r.log.Warn("recorder.cleanup: hwi is 0, skipping waveIn calls")
	} else {
		for i := range numBufs {
			if r.hdrs[i].LpData == 0 {
				r.log.Warn("recorder.cleanup: skipping header with nil LpData", "buf", i)
				continue
			}
			ret, _, _ := pWaveInUnprepHdr.Call(r.hwi, uintptr(unsafe.Pointer(&r.hdrs[i])), unsafe.Sizeof(r.hdrs[i]))
			if ret != 0 {
				r.log.Error("recorder.cleanup: waveInUnprepareHeader failed", "buf", i, "mmresult", ret)
			}
		}
		ret, _, _ := pWaveInClose.Call(r.hwi)
		if ret != 0 {
			r.log.Error("recorder.cleanup: waveInClose failed", "mmresult", ret)
		}
		r.hwi = 0
	}

	if r.event != 0 {
		windows.CloseHandle(r.event)
		r.event = 0
	}
	r.log.Info("recorder.cleanup: done")
}
