package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/energye/systray"
)

type trayState int

const (
	stateIdle trayState = iota
	stateListening
	stateTranscribing
)

// makeICO generates a 16x16 32-bit ICO with a filled circle.
func makeICO(r, g, b, a byte) []byte {
	const size = 16
	var buf bytes.Buffer

	binary.Write(&buf, binary.LittleEndian, uint16(0))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint16(1))

	pixelData := size * size * 4
	andMask := size * 4
	imgSize := uint32(40 + pixelData + andMask)
	buf.WriteByte(size)
	buf.WriteByte(size)
	buf.WriteByte(0)
	buf.WriteByte(0)
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint16(32))
	binary.Write(&buf, binary.LittleEndian, imgSize)
	binary.Write(&buf, binary.LittleEndian, uint32(22))

	binary.Write(&buf, binary.LittleEndian, uint32(40))
	binary.Write(&buf, binary.LittleEndian, int32(size))
	binary.Write(&buf, binary.LittleEndian, int32(size*2))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint16(32))
	binary.Write(&buf, binary.LittleEndian, uint32(0))
	binary.Write(&buf, binary.LittleEndian, uint32(pixelData+andMask))
	binary.Write(&buf, binary.LittleEndian, int32(0))
	binary.Write(&buf, binary.LittleEndian, int32(0))
	binary.Write(&buf, binary.LittleEndian, uint32(0))
	binary.Write(&buf, binary.LittleEndian, uint32(0))

	cx, cy := float64(size-1)/2, float64(size-1)/2
	radius := float64(size)/2 - 1
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx := float64(x) - cx
			dy := float64(y) - cy
			dist := math.Sqrt(dx*dx + dy*dy)
			if dist <= radius-0.5 {
				buf.Write([]byte{b, g, r, a})
			} else if dist <= radius+0.5 {
				aa := byte(float64(a) * (radius + 0.5 - dist))
				buf.Write([]byte{b, g, r, aa})
			} else {
				buf.Write([]byte{0, 0, 0, 0})
			}
		}
	}

	buf.Write(make([]byte, andMask))
	return buf.Bytes()
}

func setupTray(svc *sttService, backend string, log *slog.Logger) {
	// Load custom icon for idle state, fall back to generated circle
	exe, _ := os.Executable()
	iconPath := filepath.Join(filepath.Dir(exe), "icon.ico")
	iconIdle, err := os.ReadFile(iconPath)
	if err != nil {
		log.Warn("[SVC] Could not load icon.ico, using fallback", "err", err)
		iconIdle = makeICO(128, 128, 128, 255)
	}
	iconListen := makeICO(76, 175, 80, 255)
	iconTranscribe := makeICO(255, 152, 0, 255)
	iconOffline := makeICO(244, 67, 54, 255) // red — local sidecar unreachable

	// Tray-icon color is the only colored surface available: energye/systray
	// menu items can't carry icons, so menu text stays monochrome and the live
	// local-model health is reflected in the tray icon while idle.
	var localOffline atomic.Bool
	var lastTrayState atomic.Int32
	idleIcon := func() []byte {
		if svc.backend == "whisper_local" && localOffline.Load() {
			return iconOffline
		}
		return iconIdle
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Start clipboard paste-path hotkey (Ctrl+Shift+V)
	go runClipboardHotkey(ctx, log)

	systray.Run(func() {
		systray.SetIcon(iconIdle)
		setTrayStateTitle(stateIdle)
		systray.SetTooltip("STT-Go: Idle")

		backendLabel := map[string]string{
			"deepgram":         "Deepgram Nova-3",
			"api":              "Whisper",
			"elevenlabs":       "ElevenLabs Scribe",
			"whisper_stream":   "Whisper (streaming)",
			"whisper_realtime": "Whisper (realtime)",
			"groq":             "Groq Whisper",
		}[backend]
		if backendLabel == "" {
			backendLabel = backend
		}
		mInfo := systray.AddMenuItem(fmt.Sprintf("STT-Go (%s)", backendLabel), "")
		mInfo.Disable()

		// Live status of the local Whisper sidecar (sidecar/server.py on 127.0.0.1:5111).
		// Polled in the background so the user can see at a glance whether the
		// GPU model is reachable without having to dictate and watch it fail.
		mLocalStatus := systray.AddMenuItem("Local model: checking…", "Local Whisper GPU sidecar (127.0.0.1:5111)")
		mLocalStatus.Disable()
		go func() {
			check := func() {
				hctx, hcancel := context.WithTimeout(ctx, 2*time.Second)
				defer hcancel()
				reachable, loaded := localWhisperHealth(hctx)
				localOffline.Store(!reachable)
				switch {
				case !reachable:
					mLocalStatus.SetTitle("Local model: offline")
				case loaded:
					mLocalStatus.SetTitle("Local model: connected")
				default:
					mLocalStatus.SetTitle("Local model: idle (not loaded)")
				}
				if trayState(lastTrayState.Load()) == stateIdle {
					systray.SetIcon(idleIcon())
				}
			}
			check()
			t := time.NewTicker(4 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					check()
				}
			}
		}()

		// Manual recovery: kill any stale sidecar, respawn it, and reload the
		// model. Lets the user unstick the local backend without restarting the app.
		mRestartLocal := systray.AddMenuItem("Restart local model", "Kill and respawn the Whisper GPU sidecar")
		mRestartLocal.Click(func() {
			mLocalStatus.SetTitle("Local model: restarting…")
			restartLocalWhisperSidecar(log)
		})

		// Microphone submenu
		mMicMenu := systray.AddMenuItem("Microphone", "Select input device")
		mics := listMics()
		var micItems []*systray.MenuItem
		activeDeviceID := svc.rec.deviceID

		for _, mic := range mics {
			item := mMicMenu.AddSubMenuItem(mic.Name, "")
			if mic.ID == activeDeviceID {
				item.Check()
			}
			micID := mic.ID
			micName := mic.Name
			item.Click(func() {
				// Uncheck all, check selected
				for _, mi := range micItems {
					mi.Uncheck()
				}
				item.Check()
				svc.rec.setDeviceID(micID)
				log.Info("[CFG] Switched microphone", "device", micID, "name", micName)
				// Persist selection to config
				appConfig.MicDevice = micName
				if err := saveConfig(appConfig); err != nil {
					log.Error("[CFG] Failed to save mic preference", "err", err)
				}
			})
			micItems = append(micItems, item)
		}
		if len(mics) == 0 {
			noMic := mMicMenu.AddSubMenuItem("No microphones found", "")
			noMic.Disable()
		}

		// Backend submenu
		mBackendMenu := systray.AddMenuItem("Backend", "Select transcription backend")
		mDeepgram := mBackendMenu.AddSubMenuItem("Deepgram Nova-3", "")
		mElevenLabs := mBackendMenu.AddSubMenuItem("ElevenLabs Scribe (streaming)", "")
		mElevenLabsBatch := mBackendMenu.AddSubMenuItem("ElevenLabs Scribe (batch + keyterms)", "")
		mWhisper := mBackendMenu.AddSubMenuItem("Whisper (OpenAI)", "")
		mWhisperStream := mBackendMenu.AddSubMenuItem("Whisper (streaming)", "")
		mWhisperRealtime := mBackendMenu.AddSubMenuItem("Whisper (realtime)", "")
		mGroq := mBackendMenu.AddSubMenuItem("Groq Whisper", "")
		mWhisperLocal := mBackendMenu.AddSubMenuItem("Whisper Local (GPU)", "")
		switch backend {
		case "deepgram":
			mDeepgram.Check()
		case "elevenlabs":
			mElevenLabs.Check()
		case "elevenlabs_batch":
			mElevenLabsBatch.Check()
		case "whisper_stream":
			mWhisperStream.Check()
		case "whisper_realtime":
			mWhisperRealtime.Check()
		case "groq":
			mGroq.Check()
		case "whisper_local":
			mWhisperLocal.Check()
		default:
			mWhisper.Check()
		}
		uncheckAllBackends := func() {
			mDeepgram.Uncheck()
			mElevenLabs.Uncheck()
			mElevenLabsBatch.Uncheck()
			mWhisper.Uncheck()
			mWhisperStream.Uncheck()
			mWhisperRealtime.Uncheck()
			mGroq.Uncheck()
			mWhisperLocal.Uncheck()
		}
		mDeepgram.Click(func() {
			uncheckAllBackends()
			mDeepgram.Check()
			svc.switchBackend("deepgram")
			mInfo.SetTitle("STT-Go (Deepgram Nova-3)")
		})
		mElevenLabs.Click(func() {
			uncheckAllBackends()
			mElevenLabs.Check()
			svc.switchBackend("elevenlabs")
			mInfo.SetTitle("STT-Go (ElevenLabs Scribe)")
		})
		mElevenLabsBatch.Click(func() {
			uncheckAllBackends()
			mElevenLabsBatch.Check()
			svc.switchBackend("elevenlabs_batch")
			mInfo.SetTitle("STT-Go (ElevenLabs batch)")
		})
		mWhisper.Click(func() {
			uncheckAllBackends()
			mWhisper.Check()
			svc.switchBackend("api")
			mInfo.SetTitle("STT-Go (Whisper)")
		})
		mWhisperStream.Click(func() {
			uncheckAllBackends()
			mWhisperStream.Check()
			svc.switchBackend("whisper_stream")
			mInfo.SetTitle("STT-Go (Whisper streaming)")
		})
		mWhisperRealtime.Click(func() {
			uncheckAllBackends()
			mWhisperRealtime.Check()
			svc.switchBackend("whisper_realtime")
			mInfo.SetTitle("STT-Go (Whisper realtime)")
		})
		mGroq.Click(func() {
			uncheckAllBackends()
			mGroq.Check()
			svc.switchBackend("groq")
			mInfo.SetTitle("STT-Go (Groq Whisper)")
		})
		mWhisperLocal.Click(func() {
			uncheckAllBackends()
			mWhisperLocal.Check()
			svc.switchBackend("whisper_local")
			mInfo.SetTitle("STT-Go (Whisper Local GPU)")
		})

		systray.AddSeparator()
		mStreaming := systray.AddMenuItem("Real-time streaming", "Stream audio live as you speak")
		if appConfig.StreamingMode {
			mStreaming.Check()
		}
		mStreaming.Click(func() {
			appConfig.StreamingMode = !appConfig.StreamingMode
			if appConfig.StreamingMode {
				mStreaming.Check()
			} else {
				mStreaming.Uncheck()
			}
			if err := saveConfig(appConfig); err != nil {
				log.Error("[CFG] Failed to save streaming mode", "err", err)
			}
			log.Info("[CFG] Streaming mode toggled", "enabled", appConfig.StreamingMode)
		})

		systray.AddSeparator()
		mRestart := systray.AddMenuItem("Restart", "Restart STT-Go")
		mRestart.Click(func() {
			log.Info("[SVC] Restart requested from tray")
			exe, _ := os.Executable()
			cancel()
			closeRealtimePool()
			// Launch a new instance before quitting
			args := []string{"-backend", svc.backend}
			proc, err := os.StartProcess(exe, append([]string{exe}, args...), &os.ProcAttr{
				Dir:   filepath.Dir(exe),
				Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
			})
			if err != nil {
				log.Error("[SVC] Failed to restart", "err", err)
			} else {
				proc.Release()
				log.Info("[SVC] New instance launched, exiting current")
			}
			systray.Quit()
		})
		mQuit := systray.AddMenuItem("Quit", "Exit STT-Go")
		mQuit.Click(func() {
			cancel()
			closeRealtimePool()
			systray.Quit()
		})

		svc.onState = func(state trayState) {
			lastTrayState.Store(int32(state))
			switch state {
			case stateIdle:
				systray.SetIcon(idleIcon())
				setTrayStateTitle(stateIdle)
				systray.SetTooltip("STT-Go: Idle")
			case stateListening:
				systray.SetIcon(iconListen)
				setTrayStateTitle(stateListening)
				systray.SetTooltip("STT-Go: Listening...")
			case stateTranscribing:
				systray.SetIcon(iconTranscribe)
				setTrayStateTitle(stateTranscribing)
				systray.SetTooltip("STT-Go: Transcribing...")
			}
		}

		svc.run(ctx)
		systray.Quit()
	}, func() {
		log.Info("[SVC] STT-Go exiting")
	})
}
