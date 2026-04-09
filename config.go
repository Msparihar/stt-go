//go:build windows

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Config holds all user-configurable settings for stt-go.
type Config struct {
	DefaultBackend string            `json:"default_backend"` // "deepgram", "elevenlabs", "api"
	Language       string            `json:"language"`
	Keyterms       []string          `json:"keyterms"`
	Replacements   map[string]string `json:"replacements"` // from -> to
	APIKeys        struct {
		Deepgram   string `json:"deepgram"`
		OpenAI     string `json:"openai"`
		ElevenLabs string `json:"elevenlabs"`
	} `json:"api_keys"`
}

// configPath returns the path to config.json next to the exe.
func configPath() string {
	exe, _ := os.Executable()
	return filepath.Join(filepath.Dir(exe), "config.json")
}

// loadConfig loads config.json. If it doesn't exist, creates a default one and returns it.
func loadConfig(log *slog.Logger) *Config {
	path := configPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Info("config.json not found, creating default config", "path", path)
			cfg := defaultConfig()
			if saveErr := saveConfig(cfg); saveErr != nil {
				log.Warn("Failed to save default config", "err", saveErr)
			}
			return cfg
		}
		log.Warn("Failed to read config.json, using defaults", "err", err)
		return defaultConfig()
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Warn("Failed to parse config.json, using defaults", "err", err)
		return defaultConfig()
	}

	// Fill in missing fields with defaults
	def := defaultConfig()
	if cfg.DefaultBackend == "" {
		cfg.DefaultBackend = def.DefaultBackend
	}
	if cfg.Language == "" {
		cfg.Language = def.Language
	}
	if len(cfg.Keyterms) == 0 {
		cfg.Keyterms = def.Keyterms
	}
	if cfg.Replacements == nil {
		cfg.Replacements = def.Replacements
	}

	log.Info("Config loaded", "path", path, "backend", cfg.DefaultBackend, "language", cfg.Language)
	return &cfg
}

// saveConfig writes config.json with indentation.
func saveConfig(cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0644)
}

// defaultConfig returns a Config with generic developer defaults.
func defaultConfig() *Config {
	cfg := &Config{
		DefaultBackend: "deepgram",
		Language:       "en",
		Keyterms: []string{
			// AI / LLM
			"OpenAI", "Claude", "Anthropic", "GPT-4", "Whisper", "Deepgram",
			"ElevenLabs", "Codex", "Context7", "Scribe",
			// Infrastructure
			"GitHub", "GitLab", "WinSCP", "WSL", "Ubuntu", "Docker", "Kubernetes",
			"Traefik", "Dokploy", "Cloudflare", "Vercel", "Supabase", "PostgreSQL", "Redis",
			"SQLite", "MongoDB", "Hostinger", "PM2", "nginx", "systemd",
			// Languages & frameworks
			"TypeScript", "JavaScript", "Golang", "Next.js", "Svelte",
			"Zustand", "React Query", "Tailwind CSS", "Prisma", "Drizzle", "tRPC",
			"GraphQL", "WebSocket", "gRPC", "FastAPI", "Express",
			// Tools & package managers
			"bun", "pnpm", "uv", "cargo", "npm",
			"ESLint", "Prettier", "Biome",
			"Figma", "Linear", "htop", "tmux", "neovim",
			// Config files
			"CLAUDE.md", ".env.local", "tsconfig", "go.mod",
			// Windows / native
			"Direct2D", "Direct3D", "Win32", "GDI", "syscall", "vtable", "HWND",
			"PowerShell", "IPv4", "IPv6",
			// STT-Go specific
			"systray", "energye", "waveIn", "zshrc", "bashrc", "stt-go", "lumberjack",
			// Claude Code / workflow
			"Anthropic API", "MCP", "subagent", "worktree",
			"Haiku", "Claude Code", "PRD", "EPUB", "CodeRabbit",
			// Workflow terms
			"sprint", "standup", "hotfix", "backlog",
			// Commonly misheard
			"revamp", "go ahead", "stringify", "JSON.stringify",
			"Haiku model", "Claude Haiku", "preview URL", "key terms",
		},
		Replacements: map[string]string{
			"high key":              "Haiku",
			"high-key":              "Haiku",
			"highkey":               "Haiku",
			"PV URL":                "preview URL",
			"P.V. URL":              "preview URL",
			"pay rate":              "Playwright",
			"play rate":             "Playwright",
			"code debit commands":   "CodeRabbit comments",
			"code drabbit comments": "CodeRabbit comments",
			"code rabbit":           "CodeRabbit",
			"Codex comments":        "CodeRabbit comments",
			"11 labs":               "ElevenLabs",
			"11labs":                "ElevenLabs",
			"eleven labs":           "ElevenLabs",
		},
	}
	return cfg
}

// loadReplacements converts the config map to the slice format used by postProcess.
func loadReplacements(m map[string]string) []struct{ from, to string } {
	result := make([]struct{ from, to string }, 0, len(m))
	for from, to := range m {
		result = append(result, struct{ from, to string }{from, to})
	}
	return result
}

// runSetup runs an interactive CLI setup wizard to configure stt-go.
func runSetup(log *slog.Logger) {
	reader := bufio.NewReader(os.Stdin)

	readLine := func(prompt string) string {
		fmt.Print(prompt)
		line, _ := reader.ReadString('\n')
		return strings.TrimSpace(line)
	}

	fmt.Println("===========================================")
	fmt.Println("  STT-Go Setup")
	fmt.Println("===========================================")
	fmt.Println()

	cfg := defaultConfig()

	// Ask backend
	fmt.Println("Available backends:")
	fmt.Println("  1. deepgram   — Deepgram Nova-3 (recommended, fast streaming)")
	fmt.Println("  2. elevenlabs — ElevenLabs Scribe (high accuracy)")
	fmt.Println("  3. api        — OpenAI Whisper (offline-friendly, slower)")
	fmt.Println()
	backendInput := readLine("Default backend [deepgram]: ")
	switch backendInput {
	case "elevenlabs", "2":
		cfg.DefaultBackend = "elevenlabs"
	case "api", "whisper", "3":
		cfg.DefaultBackend = "api"
	default:
		cfg.DefaultBackend = "deepgram"
	}
	fmt.Printf("Using backend: %s\n\n", cfg.DefaultBackend)

	// Ask for API keys based on backend
	fmt.Println("Enter API keys (press Enter to skip):")
	fmt.Println("Keys are stored in config.json next to the exe.")
	fmt.Println()

	switch cfg.DefaultBackend {
	case "deepgram":
		key := readLine("Deepgram API key: ")
		if key != "" {
			cfg.APIKeys.Deepgram = key
		}
	case "elevenlabs":
		key := readLine("ElevenLabs API key: ")
		if key != "" {
			cfg.APIKeys.ElevenLabs = key
		}
	}

	// Always ask for OpenAI — used for Whisper fallback
	openaiKey := readLine("OpenAI API key (used for Whisper fallback): ")
	if openaiKey != "" {
		cfg.APIKeys.OpenAI = openaiKey
	}

	// Save config
	if err := saveConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Failed to save config: %v\n", err)
		log.Error("Setup failed to save config", "err", err)
		return
	}

	fmt.Println()
	fmt.Println("===========================================")
	fmt.Printf("Config saved to: %s\n", configPath())
	fmt.Println()
	fmt.Println("Run stt-go.exe to start.")
	fmt.Println("===========================================")
	log.Info("Setup complete", "config", configPath(), "backend", cfg.DefaultBackend)
}
