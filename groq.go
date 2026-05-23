//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

const groqTranscribeURL = "https://api.groq.com/openai/v1/audio/transcriptions"

func transcribeGroq(ctx context.Context, apiKey string, pcm []byte, log *slog.Logger) (string, error) {
	t0 := time.Now()
	duration := float64(len(pcm)) / float64(avgBytesPerSec)
	model := appConfig.GroqModel
	if model == "" {
		model = "whisper-large-v3"
	}
	log.Info("[GROQ] request start", "model", model, "duration", fmt.Sprintf("%.1fs", duration))

	wav := pcmToWAV(pcm)

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, _ := w.CreateFormFile("file", "audio.wav")
	fw.Write(wav)
	w.WriteField("model", model)
	w.WriteField("response_format", "json")
	w.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", groqTranscribeURL, &body)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := groqHTTPClient.Do(req)
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
		log.Error("[GROQ] unmarshal failed", "err", err, "body", fmt.Sprintf("%.200q", string(rb)))
		return "", fmt.Errorf("unmarshal response: %w", err)
	}

	text := strings.TrimSpace(res.Text)
	snippet := text
	if len(snippet) > 80 {
		snippet = snippet[:80] + "..."
	}
	log.Info("[GROQ] response received", "elapsed", time.Since(t0).Round(time.Millisecond), "text_len", len(text), "text", snippet)
	return text, nil
}
