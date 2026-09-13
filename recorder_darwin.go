//go:build darwin

package main

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gen2brain/malgo"
)

// ── Mic enumeration ────────────────────────────────────────────────

type micDevice struct {
	ID   uintptr // index into the capture-device enumeration
	Name string
}

func listMics() []micDevice {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		slog.Error("[REC] listMics: InitContext failed", "err", err)
		return nil
	}
	defer func() { _ = ctx.Uninit(); ctx.Free() }()

	infos, err := ctx.Devices(malgo.Capture)
	if err != nil {
		slog.Error("[REC] listMics: enumeration failed", "err", err)
		return nil
	}
	var mics []micDevice
	for i, info := range infos {
		mics = append(mics, micDevice{ID: uintptr(i), Name: info.Name()})
	}
	slog.Info("[REC] listMics: enumeration complete", "total", len(mics))
	return mics
}

// ── Audio recorder (miniaudio via malgo) ───────────────────────────

type recorder struct {
	ctx        *malgo.AllocatedContext
	device     *malgo.Device
	mu         sync.Mutex
	running    bool
	deviceName string

	allData    []byte
	byteCount  int
	chunkCount int
	onChunk    func([]byte)
	log        *slog.Logger
}

func newRecorder(log *slog.Logger) *recorder {
	r := &recorder{log: log}
	r.log.Info("[REC] newRecorder: created", "device", "system default")
	return r
}

func (r *recorder) setDeviceName(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log.Info("[REC] Device changed", "old", r.deviceName, "new", name)
	r.deviceName = name
}

func (r *recorder) start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return nil
	}

	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return fmt.Errorf("malgo InitContext: %w", err)
	}
	r.ctx = ctx

	cfg := malgo.DefaultDeviceConfig(malgo.Capture)
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = audioCh
	cfg.SampleRate = sampleRate
	cfg.PeriodSizeInMilliseconds = bufDurationMs

	// Device enumeration order changes whenever Bluetooth devices connect or
	// disconnect. Resolve the saved name against the fresh enumeration for every
	// recording instead of retaining a stale numeric index.
	if r.deviceName != "" {
		infos, listErr := ctx.Devices(malgo.Capture)
		if listErr != nil {
			r.log.Warn("[REC] start: microphone enumeration failed, using system default", "name", r.deviceName, "err", listErr)
		} else {
			found := false
			for _, info := range infos {
				if info.Name() != r.deviceName {
					continue
				}
				id := info.ID
				cfg.Capture.DeviceID = id.Pointer()
				r.log.Info("[REC] start: using selected mic", "name", info.Name())
				found = true
				break
			}
			if !found {
				r.log.Warn("[REC] start: selected microphone not found, using system default", "name", r.deviceName)
			}
		}
	}

	r.allData = nil
	r.byteCount = 0
	r.chunkCount = 0

	onRecv := func(_, pcm []byte, _ uint32) {
		if len(pcm) == 0 {
			return
		}
		data := make([]byte, len(pcm))
		copy(data, pcm)

		r.byteCount += len(data)
		r.chunkCount++
		if r.chunkCount%10 == 0 {
			r.log.Info("[REC] onRecv: progress", "chunks", r.chunkCount, "totalBytes", r.byteCount)
		}

		if r.onChunk != nil {
			r.onChunk(data)
		} else {
			r.allData = append(r.allData, data...)
		}
	}

	device, err := malgo.InitDevice(ctx.Context, cfg, malgo.DeviceCallbacks{Data: onRecv})
	if err != nil {
		_ = ctx.Uninit()
		ctx.Free()
		r.ctx = nil
		return fmt.Errorf("malgo InitDevice: %w", err)
	}
	r.device = device

	if err := device.Start(); err != nil {
		device.Uninit()
		_ = ctx.Uninit()
		ctx.Free()
		r.device, r.ctx = nil, nil
		return fmt.Errorf("malgo device Start: %w", err)
	}

	r.running = true
	r.log.Info("[REC] Recording started")
	return nil
}

func (r *recorder) stop() (pcm []byte, total int) {
	// Drain: keep recording briefly after key release to capture
	// trailing speech still in the mic buffer.
	r.log.Info("[REC] stop: drain started")
	time.Sleep(200 * time.Millisecond)

	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return r.allData, r.byteCount
	}
	r.running = false

	if r.device != nil {
		r.device.Uninit() // stops the device and the callback stream
		r.device = nil
	}
	if r.ctx != nil {
		_ = r.ctx.Uninit()
		r.ctx.Free()
		r.ctx = nil
	}

	r.log.Info("[REC] Recording stopped", "bytes", r.byteCount, "chunks", r.chunkCount,
		"duration", fmt.Sprintf("%.1fs", float64(r.byteCount)/float64(avgBytesPerSec)))
	return r.allData, r.byteCount
}
