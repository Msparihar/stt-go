package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"
)

// ── Retry configuration ─────────────────────────────────────────────
//
// Per-backend retry policy for REST transcription calls. Each backend
// (Whisper, ElevenLabs REST) runs its own retry loop independently —
// a failure in one does not affect the other. The race in service.go
// cancels the remaining loops once a winner is chosen.

type retryConfig struct {
	maxAttempts       int             // upper bound on attempts (including the first)
	backoffs          []time.Duration // sleep before attempts 2..maxAttempts (indexed by attempt-1)
	perAttemptTimeout time.Duration   // hard cap on a single HTTP call
	totalBudget       time.Duration   // hard cap across all attempts + backoffs
	// onRetry runs once before each retry attempt (i.e. before attempts 2..N).
	// Used by REST backends to evict poisoned keep-alive sockets via
	// http.Transport.CloseIdleConnections — without this, the same broken
	// connection is reused across retries and every attempt times out.
	onRetry func()
}

// defaultRetryConfig is what the race uses for REST backends.
// Rationale:
//   - 3 attempts matches user's stated desire ("at least three retries")
//   - 300ms / 900ms / 2.4s backoffs ride out a typical residential-ISP blip
//     (1-3s outages are the common case; anything longer and we give up)
//   - 8s per-attempt timeout: long enough for a slow upload of ~20s of audio
//     on a marginal connection, short enough to not stall the pipeline
//   - 20s total: roughly 3 * (8s attempt + ~1s backoff), but the budget kicks
//     in before we'd waste time on a third attempt when the first two took long
func defaultRetryConfig() retryConfig {
	return retryConfig{
		maxAttempts:       3,
		backoffs:          []time.Duration{300 * time.Millisecond, 900 * time.Millisecond, 2400 * time.Millisecond},
		perAttemptTimeout: 8 * time.Second,
		totalBudget:       20 * time.Second,
	}
}

// ── Error classification ────────────────────────────────────────────

type retryability int

const (
	// retryPermanent means this error will not improve on retry — stop.
	// Examples: 4xx (bad API key, malformed request), decode errors.
	retryPermanent retryability = iota
	// retryTransient means this error might resolve on retry — try again.
	// Examples: network errors, 5xx, 429 rate limit, timeouts.
	retryTransient
)

// classifyStatus maps an HTTP status code to retryability.
func classifyStatus(statusCode int) retryability {
	switch {
	case statusCode >= 500:
		return retryTransient // server-side problem, likely to resolve
	case statusCode == 429:
		return retryTransient // rate limit — will clear
	case statusCode == 408:
		return retryTransient // request timeout — retry
	case statusCode >= 400:
		return retryPermanent // client error — retry won't help (bad key, bad request)
	default:
		return retryPermanent // 2xx/3xx shouldn't reach here but be safe
	}
}

// classifyErr decides whether an error from an HTTP backend is worth retrying.
// It's deliberately conservative — when in doubt, classify as permanent to
// avoid retrying pathological errors forever. Transient means "I have strong
// reason to believe a retry will help". Everything else is permanent.
//
// statusCode is optional — pass 0 if the error itself carries status info
// (e.g. *httpStatusError). Pass nil err + statusCode to classify a bare status.
func classifyErr(err error, statusCode int) retryability {
	// If we have a status code and no transport error, classify purely on status
	if err == nil && statusCode > 0 {
		return classifyStatus(statusCode)
	}

	if err == nil {
		return retryPermanent // no error + no status = caller bug, don't retry
	}

	// Context errors are never retryable — the caller cancelled us on purpose.
	// DeadlineExceeded from a PER-ATTEMPT context is treated as transient by
	// the per-attempt context path wrapping it (see below), but a bare context
	// error at the top level means the overall race was cancelled.
	if errors.Is(err, context.Canceled) {
		return retryPermanent
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// A per-attempt timeout exceeded — worth retrying (next attempt gets
		// a fresh context). Overall-budget timeouts are handled separately
		// in withRetry via the deadline check, so they don't reach here.
		return retryTransient
	}

	// HTTP status errors from our backends carry the status code directly
	var statusErr *httpStatusError
	if errors.As(err, &statusErr) {
		return classifyStatus(statusErr.StatusCode)
	}

	// net.Error covers most transport failures (DNS, connect timeout, read reset)
	var netErr net.Error
	if errors.As(err, &netErr) {
		return retryTransient
	}

	// url.Error wraps HTTP transport errors — unwrap and classify inner
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		// Unwrap and recurse — the inner error is usually a net.Error
		if urlErr.Err != nil {
			return classifyErr(urlErr.Err, statusCode)
		}
		return retryTransient
	}

	// String-match common Windows network errors that don't satisfy net.Error.
	// These come through as plain *errors.errorString when bubbled up from
	// Go's syscall layer on Windows.
	msg := err.Error()
	transientMarkers := []string{
		"forcibly closed",            // wsarecv: An existing connection was forcibly closed
		"connection reset",           // ECONNRESET
		"connection refused",         // ECONNREFUSED — service bouncing
		"no such host",               // DNS blip
		"i/o timeout",                // generic Go timeout
		"tls: handshake failure",     // TLS renegotiation
		"unexpected EOF",             // connection dropped mid-response
		"broken pipe",                // EPIPE
		"network is unreachable",     // ENETUNREACH
		"tcp: connect: timeout",      // dial timeout
		"http2: server sent GOAWAY",  // HTTP/2 server shutting down a connection
	}
	lowered := strings.ToLower(msg)
	for _, marker := range transientMarkers {
		if strings.Contains(lowered, marker) {
			return retryTransient
		}
	}

	// Unknown error — be conservative, don't retry
	return retryPermanent
}

// ── Retry wrapper ───────────────────────────────────────────────────

// retryResult bundles what withRetry reports about a single attempt.
// Useful for structured logging.
type retryResult struct {
	text       string
	err        error
	attempts   int           // total attempts made (1..maxAttempts)
	totalTime  time.Duration // wall clock from first attempt to final result
	permanent  bool          // true if we stopped early due to a permanent error
}

// attemptFn runs one HTTP call. It MUST honor ctx.Done() for cancellation
// and return promptly when ctx is cancelled.
// On a non-nil error, if the error came from an HTTP response, the
// implementation should include the status code in the error message so
// classifyErr can act on it (we don't thread statusCode through the function
// signature — simpler to have the caller annotate via fmt.Errorf).
type attemptFn func(ctx context.Context) (string, error)

// withRetry runs fn up to cfg.maxAttempts times with cfg.backoffs between attempts.
// It respects ctx cancellation immediately (both between attempts and during them,
// via the per-attempt context). It stops early on permanent errors.
//
// Returns the first successful (non-empty) result, or the last error if all attempts fail.
// An empty transcript with no error is treated as success — the caller decides
// whether to accept it. This matches the current per-attempt contract.
func withRetry(ctx context.Context, cfg retryConfig, name string, log *slog.Logger, fn attemptFn) retryResult {
	start := time.Now()
	deadline := start.Add(cfg.totalBudget)

	var lastErr error
	for attempt := 1; attempt <= cfg.maxAttempts; attempt++ {
		// Check overall cancellation / budget before starting an attempt
		if err := ctx.Err(); err != nil {
			return retryResult{
				err:       err,
				attempts:  attempt - 1,
				totalTime: time.Since(start),
			}
		}
		if time.Now().After(deadline) {
			log.Warn("[RETRY] budget exhausted",
				"backend", name, "attempt", attempt, "total_budget", cfg.totalBudget)
			return retryResult{
				err:       errors.New("retry budget exhausted"),
				attempts:  attempt - 1,
				totalTime: time.Since(start),
			}
		}

		// Per-attempt context: child of caller ctx, capped at perAttemptTimeout
		// and also capped at the remaining overall budget (whichever is shorter).
		remaining := time.Until(deadline)
		attemptTimeout := cfg.perAttemptTimeout
		if remaining < attemptTimeout {
			attemptTimeout = remaining
		}
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)

		log.Info("[RETRY] attempt started",
			"backend", name, "attempt", attempt, "max", cfg.maxAttempts, "timeout", attemptTimeout)

		text, err := fn(attemptCtx)
		cancel()

		if err == nil {
			// Success (including empty text — caller decides meaning)
			return retryResult{
				text:      text,
				attempts:  attempt,
				totalTime: time.Since(start),
			}
		}

		lastErr = err
		class := classifyErr(err, 0) // status code already baked into err message by caller
		isFinal := attempt == cfg.maxAttempts || class == retryPermanent

		log.Warn("[RETRY] attempt failed",
			"backend", name,
			"attempt", attempt,
			"err", err,
			"retryable", class == retryTransient,
			"final", isFinal,
		)

		if class == retryPermanent {
			return retryResult{
				err:       err,
				attempts:  attempt,
				totalTime: time.Since(start),
				permanent: true,
			}
		}

		if attempt >= cfg.maxAttempts {
			break
		}

		// Sleep before the next attempt, but wake immediately on ctx cancellation.
		// Using attempt-1 because backoffs[0] precedes attempt 2.
		var backoff time.Duration
		if attempt-1 < len(cfg.backoffs) {
			backoff = cfg.backoffs[attempt-1]
		} else if len(cfg.backoffs) > 0 {
			backoff = cfg.backoffs[len(cfg.backoffs)-1]
		}

		// Cap the backoff at the remaining budget — no point sleeping past it
		if time.Until(deadline) < backoff {
			backoff = time.Until(deadline)
		}
		if backoff <= 0 {
			continue // let the budget check at loop top fire
		}

		log.Info("[RETRY] backing off",
			"backend", name, "next_attempt", attempt+1, "backoff", backoff)

		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return retryResult{
				err:       ctx.Err(),
				attempts:  attempt,
				totalTime: time.Since(start),
			}
		}

		// Evict poisoned keep-alive sockets before the next attempt.
		// Without this, a half-broken TCP connection (e.g. Cloudflare reset)
		// stays in the transport pool and every retry hits the same dead socket.
		if cfg.onRetry != nil {
			cfg.onRetry()
		}
	}

	return retryResult{
		err:       lastErr,
		attempts:  cfg.maxAttempts,
		totalTime: time.Since(start),
	}
}
