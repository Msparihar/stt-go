package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// cleanupOldFiles removes debug audio files and mismatch entries older than 7 days.
// Called once on startup.
func cleanupOldFiles(log *slog.Logger) {
	exe, _ := os.Executable()
	baseDir := filepath.Dir(exe)
	cutoff := time.Now().AddDate(0, 0, -7)

	// Clean debug-audio/
	debugDir := filepath.Join(baseDir, "debug-audio")
	entries, err := os.ReadDir(debugDir)
	if err == nil {
		removed := 0
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				if err := os.Remove(filepath.Join(debugDir, e.Name())); err != nil {
					log.Error("[CLEANUP] failed to remove file", "path", filepath.Join(debugDir, e.Name()), "err", err)
				}
				removed++
			}
		}
		if removed > 0 {
			log.Info("[SVC] Cleaned up old debug audio files", "removed", removed)
		}
	}

	// Clean failed-audio/
	failedDir := filepath.Join(baseDir, "failed-audio")
	entries, err = os.ReadDir(failedDir)
	if err == nil {
		removed := 0
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				if err := os.Remove(filepath.Join(failedDir, e.Name())); err != nil {
					log.Error("[CLEANUP] failed to remove file", "path", filepath.Join(failedDir, e.Name()), "err", err)
				}
				removed++
			}
		}
		if removed > 0 {
			log.Info("[SVC] Cleaned up old failed audio files", "removed", removed)
		}
	}

	// Clean old entries from mismatches.jsonl
	mismatchPath := filepath.Join(baseDir, "mismatches.jsonl")
	data, err := os.ReadFile(mismatchPath)
	if err != nil {
		return // file doesn't exist yet, that's fine
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var kept []string
	for _, line := range lines {
		if line == "" {
			continue
		}
		var entry mismatchEntry
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		t, err := time.Parse(time.RFC3339, entry.Timestamp)
		if err != nil || t.After(cutoff) {
			kept = append(kept, line)
		}
	}

	if len(kept) < len(lines) {
		removed := len(lines) - len(kept)
		os.WriteFile(mismatchPath, []byte(strings.Join(kept, "\n")+"\n"), 0644)
		log.Info("[SVC] Cleaned up old mismatch entries", "removed", removed, "kept", len(kept))
	}
}

// saveAudioToDisk saves raw PCM audio as WAV to disk when all backends fail.
// Returns the file path, or empty string on error.
func saveAudioToDisk(pcm []byte, log *slog.Logger) string {
	exe, _ := os.Executable()
	dir := filepath.Join(filepath.Dir(exe), "failed-audio")
	os.MkdirAll(dir, 0755)

	filename := fmt.Sprintf("stt-%s.wav", time.Now().Format("2006-01-02T15-04-05"))
	path := filepath.Join(dir, filename)

	wav := pcmToWAV(pcm)
	if err := os.WriteFile(path, wav, 0644); err != nil {
		log.Error("[SVC] Failed to save audio to disk", "err", err)
		return ""
	}
	log.Info("[SVC] Audio saved to disk for later retry", "path", path, "size", len(wav))
	return path
}

// saveDebugAudio saves every recording to debug-audio/ for diagnosis.
// Returns just the filename (not full path) for compact logging.
func saveDebugAudio(pcm []byte, log *slog.Logger) string {
	exe, _ := os.Executable()
	dir := filepath.Join(filepath.Dir(exe), "debug-audio")
	os.MkdirAll(dir, 0755)

	filename := fmt.Sprintf("stt-%s.wav", time.Now().Format("2006-01-02T15-04-05.000"))
	path := filepath.Join(dir, filename)

	wav := pcmToWAV(pcm)
	if err := os.WriteFile(path, wav, 0644); err != nil {
		log.Error("[SVC] Failed to save debug audio", "err", err)
		return ""
	}
	return filename
}
