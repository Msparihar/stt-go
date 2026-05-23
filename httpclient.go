//go:build windows

package main

import (
	"net"
	"net/http"
	"time"
)

// Per-backend HTTP clients with isolated transports.
//
// Why this exists: previously every REST backend used http.DefaultClient,
// which has one shared connection pool. When Cloudflare reset a TCP
// connection mid-request, the half-broken keep-alive socket stayed in the
// pool and every subsequent request — across all backends — reused it and
// hung until per-attempt timeout. One bad blip → 24 seconds of nothing.
//
// Each backend now owns its pool. A poisoned Whisper connection cannot
// stall ElevenLabs. Combined with retry.go's CloseIdleConnections hook
// on transient errors, the pool is also evicted before the next attempt
// so retries get a fresh socket instead of reusing the broken one.

func newSTTHTTPClient() *http.Client {
	t := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 15 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       20 * time.Second,
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   2,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		Transport: t,
		// No top-level Timeout — per-attempt context in withRetry controls deadline
	}
}

// closeIdleConns calls CloseIdleConnections on the client's transport if it
// is the standard *http.Transport. Used as the OnRetry hook to evict any
// poisoned keep-alive sockets before the next attempt.
func closeIdleConns(c *http.Client) {
	if c == nil {
		return
	}
	if t, ok := c.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}

var (
	whisperHTTPClient        = newSTTHTTPClient()
	whisperStreamHTTPClient  = newSTTHTTPClient()
	elevenLabsRESTHTTPClient = newSTTHTTPClient()
	groqHTTPClient           = newSTTHTTPClient()
)
