//go:build windows

package main

import (
	"bytes"
	"context"
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

	failCount := 0
	for i, ch := range text {
		var inp [2]kbInput
		inp[0] = kbInput{typ: inputKbd, scan: uint16(ch), flags: kfUnicode}
		inp[1] = kbInput{typ: inputKbd, scan: uint16(ch), flags: kfUnicode | kfKeyup}
		ret, _, _ := pSendInput.Call(2, uintptr(unsafe.Pointer(&inp[0])), unsafe.Sizeof(inp[0]))
		if ret == 0 {
			failCount++
			if failCount <= 5 { // log first 5 failures to avoid spam
				log.Error("[TYPE] typeText: SendInput failed", "charIndex", i, "char", string(ch), "charCode", int(ch))
			}
		}
		time.Sleep(time.Millisecond)
	}

	if failCount > 0 {
		log.Error("[TYPE] typeText: SendInput failures", "failed", failCount, "total", len([]rune(text)))
	} else {
		log.Info("[TYPE] typeText: completed successfully", "chars", len([]rune(text)))
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

// transcribeWhisper calls OpenAI Whisper once. For retries, wrap in withRetry.
// The ctx controls cancellation — if cancelled, the in-flight HTTP call is
// aborted promptly.
func transcribeWhisper(ctx context.Context, pcm []byte, apiKey, lang string, log *slog.Logger) (string, error) {
	t0 := time.Now()
	duration := float64(len(pcm)) / float64(avgBytesPerSec)
	log.Info("[WH] Whisper: preparing audio", "pcmBytes", len(pcm), "duration", fmt.Sprintf("%.1fs", duration))
	wav := pcmToWAV(pcm)

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, _ := w.CreateFormFile("file", "audio.wav")
	fw.Write(wav)
	w.WriteField("model", "whisper-1")
	w.WriteField("language", lang)
	w.WriteField("prompt", whisperPrompt)
	w.Close()

	const whisperURL = "https://api.openai.com/v1/audio/transcriptions"
	log.Info("[WH] Whisper: sending HTTP request", "url", whisperURL, "contentLength", body.Len())
	req, err := http.NewRequestWithContext(ctx, "POST", whisperURL, &body)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := whisperHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", &httpStatusError{StatusCode: resp.StatusCode, Body: string(rb)}
	}

	var res struct{ Text string }
	json.Unmarshal(rb, &res)
	text := strings.TrimSpace(res.Text)
	log.Info("[WH] Whisper API", "elapsed", time.Since(t0).Round(time.Millisecond), "text", text)
	return text, nil
}

// transcribeElevenLabsREST calls ElevenLabs Scribe v2 REST API once.
// For retries, wrap in withRetry.
func transcribeElevenLabsREST(ctx context.Context, pcm []byte, apiKey, lang string, log *slog.Logger) (string, error) {
	t0 := time.Now()
	duration := float64(len(pcm)) / float64(avgBytesPerSec)
	log.Info("[EL] ElevenLabs REST: preparing audio", "pcmBytes", len(pcm), "duration", fmt.Sprintf("%.1fs", duration))
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

	const elevenLabsURL = "https://api.elevenlabs.io/v1/speech-to-text"
	log.Info("[EL] ElevenLabs REST: sending HTTP request", "url", elevenLabsURL, "contentLength", body.Len(), "keyterms", len(techTerms))
	req, err := http.NewRequestWithContext(ctx, "POST", elevenLabsURL, &body)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("xi-api-key", apiKey)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := elevenLabsRESTHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", &httpStatusError{StatusCode: resp.StatusCode, Body: string(rb)}
	}

	var res struct{ Text string }
	json.Unmarshal(rb, &res)
	text := strings.TrimSpace(res.Text)
	log.Info("[EL] ElevenLabs REST API", "elapsed", time.Since(t0).Round(time.Millisecond), "text", text)
	return text, nil
}

// httpStatusError is returned by REST backends on non-2xx responses so that
// classifyErr can distinguish transient (5xx/429) from permanent (4xx) failures.
type httpStatusError struct {
	StatusCode int
	Body       string
}

func (e *httpStatusError) Error() string {
	// Truncate body to keep error strings manageable in logs
	body := e.Body
	if len(body) > 200 {
		body = body[:200] + "..."
	}
	return fmt.Sprintf("API %d: %s", e.StatusCode, body)
}
