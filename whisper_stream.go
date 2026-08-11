package main

import (
	"bufio"
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

// transcribeWhisperStream calls OpenAI gpt-4o-mini-transcribe with stream=true,
// parses Server-Sent Events, accumulates deltas, and returns the final transcript.
// Partial deltas are logged as they arrive. The ctx controls cancellation.
func transcribeWhisperStream(ctx context.Context, pcm []byte, apiKey, lang string, log *slog.Logger) (string, error) {
	t0 := time.Now()
	duration := float64(len(pcm)) / float64(avgBytesPerSec)
	log.Info("[WH-STREAM] preparing audio", "pcmBytes", len(pcm), "duration", fmt.Sprintf("%.1fs", duration))
	wav := pcmToWAV(pcm)

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, _ := w.CreateFormFile("file", "audio.wav")
	fw.Write(wav)
	w.WriteField("model", "gpt-transcribe")
	w.WriteField("language", lang)
	w.WriteField("stream", "true")
	w.WriteField("prompt", whisperPrompt)
	w.Close()

	const whisperURL = "https://api.openai.com/v1/audio/transcriptions"
	log.Info("[WH-STREAM] sending HTTP request", "url", whisperURL, "contentLength", body.Len())
	req, err := http.NewRequestWithContext(ctx, "POST", whisperURL, &body)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := whisperStreamHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		rb, _ := io.ReadAll(resp.Body)
		return "", &httpStatusError{StatusCode: resp.StatusCode, Body: string(rb)}
	}

	// Parse SSE stream
	var deltaBuffer strings.Builder
	var finalText string
	scanner := bufio.NewScanner(resp.Body)

	for scanner.Scan() {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}

		line := scanner.Text()

		// SSE lines are either "data: {json}" or empty (separator)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}

		var event struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
			Text  string `json:"text"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			log.Warn("[WH-STREAM] failed to parse SSE event", "payload", payload, "err", err)
			continue
		}

		switch event.Type {
		case "transcript.text.delta":
			deltaBuffer.WriteString(event.Delta)
			log.Info("[WH-STREAM] partial delta", "delta", event.Delta, "accumulated", deltaBuffer.String())
		case "transcript.text.done":
			finalText = strings.TrimSpace(event.Text)
			log.Info("[WH-STREAM] final text received", "text", finalText)
		}
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("SSE read: %w", err)
	}

	// Prefer the explicit done event text; fall back to accumulated deltas
	if finalText == "" {
		finalText = strings.TrimSpace(deltaBuffer.String())
	}

	log.Info("[WH-STREAM] completed", "elapsed", time.Since(t0).Round(time.Millisecond), "text", finalText)
	return finalText, nil
}
