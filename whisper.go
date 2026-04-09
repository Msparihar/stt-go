//go:build windows

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
	"unsafe"
)

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
		st, _, _ := pGetAsyncKey.Call(vkRMenu)
		if int16(st) >= 0 {
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
		log.Info("typeText: will type", "chars", len(text), "text", text[:80]+"...")
	} else {
		log.Info("typeText: will type", "chars", len(text), "text", text)
	}

	// Wait for Right Alt to be released so SendInput isn't swallowed
	waitForRightAltRelease()

	// Restore the window that was focused when recording started
	if targetHwnd != 0 {
		currentHwnd, _, _ := pGetForegroundWindow.Call()
		if currentHwnd != targetHwnd {
			log.Info("typeText: restoring foreground window", "target", fmt.Sprintf("0x%X", targetHwnd), "current", fmt.Sprintf("0x%X", currentHwnd))
			pSetForegroundWindow.Call(targetHwnd)
			time.Sleep(50 * time.Millisecond) // let window activate
		}
	}

	// Pre-type delay to let focus settle
	time.Sleep(150 * time.Millisecond)

	failCount := 0
	for i, ch := range text {
		var inp [2]kbInput
		inp[0] = kbInput{typ: inputKbd, scan: uint16(ch), flags: kfUnicode}
		inp[1] = kbInput{typ: inputKbd, scan: uint16(ch), flags: kfUnicode | kfKeyup}
		ret, _, _ := pSendInput.Call(2, uintptr(unsafe.Pointer(&inp[0])), unsafe.Sizeof(inp[0]))
		if ret == 0 {
			failCount++
			if failCount <= 5 { // log first 5 failures to avoid spam
				log.Error("typeText: SendInput failed", "charIndex", i, "char", string(ch), "charCode", int(ch))
			}
		}
		time.Sleep(time.Millisecond)
	}

	if failCount > 0 {
		log.Error("typeText: SendInput failures", "failed", failCount, "total", len([]rune(text)))
	} else {
		log.Info("typeText: completed successfully", "chars", len([]rune(text)))
	}
}

// ── WAV encoding ───────────────────────────────────────────────────

func pcmToWAV(pcm []byte) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+len(pcm)))
	b.WriteString("WAVE")
	b.WriteString("fmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint16(audioCh))
	binary.Write(&b, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&b, binary.LittleEndian, uint32(avgBytesPerSec))
	binary.Write(&b, binary.LittleEndian, uint16(blockAlign))
	binary.Write(&b, binary.LittleEndian, uint16(bitsPerSample))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}

// ── Whisper API ────────────────────────────────────────────────────

func transcribeWhisper(pcm []byte, apiKey, lang string, log *slog.Logger) (string, error) {
	t0 := time.Now()
	duration := float64(len(pcm)) / float64(avgBytesPerSec)
	log.Info("Whisper: preparing audio", "pcmBytes", len(pcm), "duration", fmt.Sprintf("%.1fs", duration))
	wav := pcmToWAV(pcm)

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, _ := w.CreateFormFile("file", "audio.wav")
	fw.Write(wav)
	w.WriteField("model", "whisper-1")
	w.WriteField("language", lang)
	w.WriteField("prompt", whisperPrompt)
	w.Close()

	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/audio/transcriptions", &body)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("API %d: %s", resp.StatusCode, rb)
	}

	var res struct{ Text string }
	json.Unmarshal(rb, &res)
	text := strings.TrimSpace(res.Text)
	log.Info("Whisper API", "elapsed", time.Since(t0).Round(time.Millisecond), "text", text)
	return text, nil
}

// transcribeElevenLabsREST calls ElevenLabs Scribe v2 REST API for non-streaming transcription.
func transcribeElevenLabsREST(pcm []byte, apiKey, lang string, log *slog.Logger) (string, error) {
	t0 := time.Now()
	duration := float64(len(pcm)) / float64(avgBytesPerSec)
	log.Info("ElevenLabs REST: preparing audio", "pcmBytes", len(pcm), "duration", fmt.Sprintf("%.1fs", duration))
	wav := pcmToWAV(pcm)

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, _ := w.CreateFormFile("file", "audio.wav")
	fw.Write(wav)
	w.WriteField("model_id", "scribe_v2")
	w.WriteField("language_code", lang)
	w.WriteField("no_verbatim", "true") // Remove filler words and false starts
	// Add tech vocabulary as keyterms for better accuracy
	for _, kt := range techTerms {
		w.WriteField("keyterms", kt)
	}
	w.Close()

	req, _ := http.NewRequest("POST", "https://api.elevenlabs.io/v1/speech-to-text", &body)
	req.Header.Set("xi-api-key", apiKey)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("API %d: %s", resp.StatusCode, rb)
	}

	var res struct{ Text string }
	json.Unmarshal(rb, &res)
	text := strings.TrimSpace(res.Text)
	log.Info("ElevenLabs REST API", "elapsed", time.Since(t0).Round(time.Millisecond), "text", text)
	return text, nil
}

// transcribeParallelFallback runs Whisper and ElevenLabs REST in parallel, returns first success.
func transcribeParallelFallback(pcm []byte, whisperKey, elevenLabsKey, lang string, log *slog.Logger) (text string, usedBackend string, err error) {
	type result struct {
		text    string
		backend string
		err     error
	}
	ch := make(chan result, 2)

	if whisperKey != "" {
		go func() {
			t, e := transcribeWhisper(pcm, whisperKey, lang, log)
			ch <- result{t, "whisper_fallback", e}
		}()
	}
	if elevenLabsKey != "" {
		go func() {
			t, e := transcribeElevenLabsREST(pcm, elevenLabsKey, lang, log)
			ch <- result{t, "elevenlabs_rest_fallback", e}
		}()
	}

	expected := 0
	if whisperKey != "" {
		expected++
	}
	if elevenLabsKey != "" {
		expected++
	}
	if expected == 0 {
		return "", "", fmt.Errorf("no fallback API keys available")
	}

	// Collect results — return first success
	var lastErr error
	for i := 0; i < expected; i++ {
		r := <-ch
		if r.err == nil && r.text != "" {
			log.Info("Parallel fallback succeeded", "backend", r.backend)
			return r.text, r.backend, nil
		}
		if r.err != nil {
			log.Warn("Parallel fallback attempt failed", "backend", r.backend, "err", r.err)
			lastErr = r.err
		}
	}
	return "", "", lastErr
}
