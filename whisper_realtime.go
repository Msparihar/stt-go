//go:build windows

package main

import (
	"context"
	"log/slog"
)

const realtimeURL = "wss://api.openai.com/v1/realtime?intent=transcription"

// transcribeWhisperRealtime is the public entry point for the whisper_realtime
// backend. It delegates to the persistent realtimeClient pool, which reuses a
// single WebSocket connection across utterances and only re-dials on failure or
// after 5 minutes of idle.
//
// Auth note: Direct API key auth is used. If the server returns 401,
// TODO: switch to ephemeral tokens via POST /v1/realtime/sessions before connecting.
func transcribeWhisperRealtime(ctx context.Context, pcm []byte, apiKey, lang string, log *slog.Logger) (string, error) {
	pool := getRealtimePool(apiKey, log)
	return pool.Transcribe(ctx, pcm, lang)
}

