package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

var noopLog = slog.New(slog.NewTextHandler(nopWriter{}, nil))

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestWithRetry_SuccessFirstCall(t *testing.T) {
	calls := 0
	cfg := defaultRetryConfig()
	res := withRetry(context.Background(), cfg, "test", noopLog, func(ctx context.Context) (string, error) {
		calls++
		return "hello", nil
	})
	if res.err != nil {
		t.Fatalf("expected no error, got %v", res.err)
	}
	if res.text != "hello" {
		t.Fatalf("expected 'hello', got %q", res.text)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
	if res.attempts != 1 {
		t.Fatalf("expected attempts=1, got %d", res.attempts)
	}
}

func TestWithRetry_PermanentErrorNoRetry(t *testing.T) {
	calls := 0
	cfg := defaultRetryConfig()
	permErr := &httpStatusError{StatusCode: 400, Body: "bad request"}
	res := withRetry(context.Background(), cfg, "test", noopLog, func(ctx context.Context) (string, error) {
		calls++
		return "", permErr
	})
	if !errors.Is(res.err, permErr) && res.err == nil {
		t.Fatalf("expected permanent error, got %v", res.err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call (no retries), got %d", calls)
	}
	if !res.permanent {
		t.Fatal("expected permanent=true")
	}
}

func TestWithRetry_TransientErrorRetries(t *testing.T) {
	calls := 0
	cfg := defaultRetryConfig()
	cfg.backoffs = []time.Duration{1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond}
	transientErr := &httpStatusError{StatusCode: 503, Body: "service unavailable"}
	res := withRetry(context.Background(), cfg, "test", noopLog, func(ctx context.Context) (string, error) {
		calls++
		return "", transientErr
	})
	if calls != cfg.maxAttempts {
		t.Fatalf("expected %d calls, got %d", cfg.maxAttempts, calls)
	}
	if res.err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if res.permanent {
		t.Fatal("expected permanent=false for transient error")
	}
}

func TestWithRetry_BudgetExceededReturnsError(t *testing.T) {
	cfg := defaultRetryConfig()
	cfg.totalBudget = 1 * time.Millisecond
	cfg.perAttemptTimeout = 50 * time.Millisecond
	cfg.backoffs = []time.Duration{100 * time.Millisecond, 100 * time.Millisecond}
	transientErr := &httpStatusError{StatusCode: 429, Body: "rate limited"}
	res := withRetry(context.Background(), cfg, "test", noopLog, func(ctx context.Context) (string, error) {
		time.Sleep(5 * time.Millisecond)
		return "", transientErr
	})
	if res.err == nil {
		t.Fatal("expected error when budget exceeded")
	}
}
