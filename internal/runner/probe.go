package runner

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// httpClient is shared across all HTTP probes to benefit from connection reuse.
var httpClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        10,
		IdleConnTimeout:      30 * time.Second,
		DisableKeepAlives:   false,
		MaxIdleConnsPerHost: 5,
	},
}

// probeHTTP polls url with GET requests until a 2xx response, ctx cancellation,
// or timeout (when timeout > 0). Returns true on 2xx, false on timeout or
// cancellation. Callers should check ctx.Err() to distinguish those two cases.
// progressFn is called every 3 seconds with elapsed duration while probe is running.
func probeHTTP(ctx context.Context, url string, timeout time.Duration, logFn func(string), progressFn func(string)) bool {
	deadline := deadlineFor(timeout)
	failures := 0
	elapsed := time.Duration(0)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	start := time.Now()

	// Log initial wait message.
	if progressFn != nil {
		progressFn(fmt.Sprintf("waiting for http probe %s...", url))
	}

	for {
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			if logFn != nil {
				logFn(fmt.Sprintf("probe http %s: bad URL: %v", url, err))
			}
			return false
		}
		resp, err := httpClient.Do(req)
		if err == nil {
			// Drain body so the connection can be reused on the next poll.
			// Without this every probe opens a fresh TCP/TLS connection.
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return true
			}
			failures++
			if logFn != nil && failures > 5 {
				logFn(fmt.Sprintf("probe http %s: status %d", url, resp.StatusCode))
			}
		} else {
			failures++
			if logFn != nil && failures > 5 {
				logFn(fmt.Sprintf("probe http %s: %v", url, err))
			}
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			if logFn != nil {
				logFn(fmt.Sprintf("probe http %s: timed out after %s", url, timeout))
			}
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			elapsed = time.Since(start)
			if progressFn != nil {
				if !deadline.IsZero() && deadline.Before(time.Now().Add(3*time.Second)) {
					// Less than 3s until deadline - skip progress update to avoid spam
					continue
				}
				remaining := ""
				if !deadline.IsZero() {
					remaining = fmt.Sprintf(" (%.0fs left)", time.Until(deadline).Seconds())
				}
				progressFn(fmt.Sprintf("waiting for http probe %s... (%.0fs elapsed)%s", url, elapsed.Seconds(), remaining))
			}
		case <-time.After(time.Second):
		}
	}
}

// probeTCP polls addr (host:port) with TCP connects until success, ctx
// cancellation, or timeout. Same return semantics as probeHTTP.
// progressFn is called every 3 seconds with elapsed duration while probe is running.
func probeTCP(ctx context.Context, addr string, timeout time.Duration, logFn func(string), progressFn func(string)) bool {
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	deadline := deadlineFor(timeout)
	failures := 0
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	start := time.Now()

	// Log initial wait message.
	if progressFn != nil {
		progressFn(fmt.Sprintf("waiting for tcp probe %s...", addr))
	}

	for {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			conn.Close()
			return true
		}
		failures++
		if logFn != nil && failures > 5 {
			logFn(fmt.Sprintf("probe tcp %s: %v", addr, err))
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			if logFn != nil {
				logFn(fmt.Sprintf("probe tcp %s: timed out after %s", addr, timeout))
			}
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if progressFn != nil {
				elapsed := time.Since(start)
				if !deadline.IsZero() && deadline.Before(time.Now().Add(3*time.Second)) {
					continue
				}
				remaining := ""
				if !deadline.IsZero() {
					remaining = fmt.Sprintf(" (%.0fs left)", time.Until(deadline).Seconds())
				}
				progressFn(fmt.Sprintf("waiting for tcp probe %s... (%.0fs elapsed)%s", addr, elapsed.Seconds(), remaining))
			}
		case <-time.After(time.Second):
		}
	}
}

// deadlineFor returns the absolute deadline for a probe, or the zero value
// when timeout <= 0 (meaning "no limit").
func deadlineFor(timeout time.Duration) time.Time {
	if timeout <= 0 {
		return time.Time{}
	}
	return time.Now().Add(timeout)
}
