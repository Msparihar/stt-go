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
)

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
	// gpt-transcribe: beat whisper-1 and both gpt-4o-transcribe variants on
	// accuracy and latency in the Aug 2026 benchmark on real dictation clips.
	w.WriteField("model", "gpt-transcribe")
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
	if err := json.Unmarshal(rb, &res); err != nil {
		log.Error("[WHISPER] unmarshal failed", "err", err, "body", fmt.Sprintf("%.200q", string(rb)))
		return "", fmt.Errorf("unmarshal response: %w", err)
	}
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
	if err := json.Unmarshal(rb, &res); err != nil {
		log.Error("[EL-REST] unmarshal failed", "err", err, "body", fmt.Sprintf("%.200q", string(rb)))
		return "", fmt.Errorf("unmarshal response: %w", err)
	}
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
