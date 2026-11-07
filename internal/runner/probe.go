package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/ipc"
)

// probeAttributor attributes the connection a probe attempt has just dialed,
// before anything is sent on it. Every attempt dials its own connection and
// is attributed this way, so the result describes the socket that will carry
// the answer while it is still open: the server cannot yet have answered,
// closed it, or left its port to a successor, and a refused connection is
// never reused by a later attempt. A nil probeAttributor attributes nothing.
type probeAttributor func(probeConn) *ipc.EndpointOwner

// probeAccept judges who answered an attempt that got an answer (nil when
// the attempt was not attributed). A non-nil error rejects the answer: the
// attempt counts as failed with that error and the poll continues. A nil
// probeAccept accepts every answer.
type probeAccept func(*ipc.EndpointOwner) error

// probeHTTP polls url with GET requests until a 2xx response that accept
// takes, ctx cancellation, or timeout (when timeout > 0). Returns true on
// success, false on timeout or cancellation. Callers should check ctx.Err()
// to distinguish those two cases. On a timeout whose last attempt was
// rejected by accept, rejected is that rejection.
// progressFn is called every 3 seconds with elapsed duration while probe is running.
func probeHTTP(ctx context.Context, url string, timeout time.Duration, logFn func(string), progressFn func(string), attribute probeAttributor, accept probeAccept) (ok bool, rejected string) {
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
		owner, err := httpOnce(ctx, url, attribute)
		rejected = ""
		if err == nil && accept != nil {
			if err = accept(owner); err != nil {
				rejected = err.Error()
			}
		}
		if err == nil {
			return true, ""
		}
		if errors.Is(err, errBadProbeURL) {
			if logFn != nil {
				logFn(fmt.Sprintf("probe http %s: %v", url, err))
			}
			return false, ""
		}
		failures++
		if logFn != nil && failures > 5 {
			logFn(fmt.Sprintf("probe http %s: %v", url, err))
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			if logFn != nil {
				logFn(fmt.Sprintf("probe http %s: timed out after %s", url, timeout))
			}
			return false, rejected
		}
		select {
		case <-ctx.Done():
			return false, ""
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

// errBadProbeURL marks an http probe URL that cannot form a request, which no
// retry can fix.
var errBadProbeURL = errors.New("bad URL")

// httpOnce sends one GET to url and returns nil on a 2xx response. It is the
// single attempt shared by the readiness poll and health checks. The request
// travels on a connection of its own (keep-alives are off and the transport
// lives for this attempt only), attributed as soon as it is dialed. The
// attribution travels on the connection itself, so it is read on this
// goroutine when the request gets that connection, and a dial that finishes
// after the request was cancelled writes nothing httpOnce reads; owner is the
// attribution of the connection that carried the answer (the last one when a
// redirect was followed). Requests never go through a proxy.
func httpOnce(ctx context.Context, url string, attribute probeAttributor) (*ipc.EndpointOwner, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil || attribute == nil {
				return conn, err
			}
			c, ok := probeConnOf(conn)
			if !ok {
				return conn, nil
			}
			return &attributedConn{Conn: conn, owner: attribute(c)}, nil
		},
	}
	defer transport.CloseIdleConnections()
	var owner *ipc.EndpointOwner
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { owner = connOwner(info.Conn) }}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errBadProbeURL, err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second, Transport: transport}).Do(req)
	if err != nil {
		return nil, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return owner, nil
	}
	return nil, fmt.Errorf("status %d", resp.StatusCode)
}

// attributedConn is a probe connection with who answered on it, attributed
// when it was dialed, before the transport could use it.
type attributedConn struct {
	net.Conn
	owner *ipc.EndpointOwner
}

// connOwner returns the attribution a connection the transport handed out
// carries: the dialed connection itself, or the one under its TLS layer.
func connOwner(conn net.Conn) *ipc.EndpointOwner {
	if tlsConn, ok := conn.(interface{ NetConn() net.Conn }); ok {
		conn = tlsConn.NetConn()
	}
	if c, ok := conn.(*attributedConn); ok {
		return c.owner
	}
	return nil
}

// tcpOnce makes one TCP connection to addr, attributes it while it is open,
// and closes it. It is the single attempt shared by the readiness poll and
// health checks.
func tcpOnce(ctx context.Context, dialer *net.Dialer, addr string, attribute probeAttributor) (*ipc.EndpointOwner, error) {
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	var owner *ipc.EndpointOwner
	if c, ok := probeConnOf(conn); ok && attribute != nil {
		owner = attribute(c)
	}
	return owner, conn.Close()
}

// probeTCP polls addr (host:port) with TCP connects until one that accept
// takes, ctx cancellation, or timeout. Same return semantics as probeHTTP.
// progressFn is called every 3 seconds with elapsed duration while probe is running.
func probeTCP(ctx context.Context, addr string, timeout time.Duration, logFn func(string), progressFn func(string), attribute probeAttributor, accept probeAccept) (ok bool, rejected string) {
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
		owner, err := tcpOnce(ctx, dialer, addr, attribute)
		rejected = ""
		if err == nil && accept != nil {
			if err = accept(owner); err != nil {
				rejected = err.Error()
			}
		}
		if err == nil {
			return true, ""
		}
		failures++
		if logFn != nil && failures > 5 {
			logFn(fmt.Sprintf("probe tcp %s: %v", addr, err))
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			if logFn != nil {
				logFn(fmt.Sprintf("probe tcp %s: timed out after %s", addr, timeout))
			}
			return false, rejected
		}
		select {
		case <-ctx.Done():
			return false, ""
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

const (
	// cmdProbeAttemptTimeout bounds one ready_when.cmd attempt, matching the
	// HTTP probe's request timeout.
	cmdProbeAttemptTimeout = 10 * time.Second
	// cmdProbeOutputLimit is how much of an attempt's output is kept per
	// stream; only the stderr head is reported.
	cmdProbeOutputLimit = 256
	// cmdProbeRetryDelay is the gap between a failed ready_when.cmd attempt
	// and the next one.
	cmdProbeRetryDelay = time.Second
)

// probeCmd runs p's ready_when.cmd until it exits 0, ctx is cancelled, or
// timeout (when > 0) passes. ok has the same semantics as probeHTTP's result.
// When timeout passes, detail describes the last attempt that ended on its
// own or at the per-attempt limit (cmdProbeFailure), for the timeout message.
// An attempt that fails with no time left for another is reported only there,
// not also through logFn.
//
// Each attempt is a quiet runner-owned execution named "<name>:probe" (':' is
// not allowed in process names, so it cannot collide) in p's dir and env. It
// runs under ctx and is bounded by handoff, so admission refuses it once
// shutdown starts and the stack seal covers its process group, like the
// process it probes.
func (r *Runner) probeCmd(ctx context.Context, p procInfo, handoff <-chan struct{}, timeout time.Duration, logFn func(string), progressFn func(string)) (ok bool, detail string) {
	deadline := deadlineFor(timeout)
	probe := cmdProbeInfo(p, p.proc.ReadyWhen.Cmd)
	if progressFn != nil {
		progressFn("waiting for cmd probe...")
	}
	failures, last := 0, ""
	for {
		attemptDeadline := time.Now().Add(cmdProbeAttemptTimeout)
		if !deadline.IsZero() && deadline.Before(attemptDeadline) {
			attemptDeadline = deadline
		}
		attemptCtx, cancel := context.WithDeadline(ctx, attemptDeadline)
		st := &procState{state: "starting", generation: 1, readyCycle: newReadyCycle(), restartCh: make(chan struct{}, 1)}
		res := r.runOwned(attemptCtx, probe, st, handoff, cmdProbeOutputLimit)
		// An attempt ended by ready_timeout itself, rather than by its own
		// exit or the per-attempt limit, says only that time ran out.
		cutByTimeout := attemptCtx.Err() != nil && attemptDeadline.Equal(deadline)
		passed := exitedZeroInTime(attemptCtx, &res)
		cancel()
		switch {
		case passed:
			return true, ""
		case res.HandedOff || ctx.Err() != nil:
			return false, ""
		case res.AdmissionRefused && r.stopping.Load():
			// Shutdown refused the attempt and is about to cancel ctx. That
			// is not a probe failure, so the verdict is left to the
			// cancellation rather than reported as a timeout. An attempt
			// refused only because its own deadline had passed falls
			// through and counts as failed.
			<-ctx.Done()
			return false, ""
		}
		failures++
		if cutByTimeout && last != "" {
			// The previous attempt's outcome is the informative one.
			return false, last
		}
		last = cmdProbeFailure(res)
		if !deadline.IsZero() {
			if remaining := time.Until(deadline); remaining <= cmdProbeRetryDelay {
				// No further attempt can start before ready_timeout, so this
				// one is reported by the timeout message instead of logged.
				// The verdict still waits for the deadline itself.
				select {
				case <-ctx.Done():
					return false, ""
				case <-time.After(remaining):
					return false, last
				}
			}
		}
		if logFn != nil && failures > 5 {
			logFn("probe cmd: " + last)
		}
		select {
		case <-ctx.Done():
			return false, ""
		case <-time.After(cmdProbeRetryDelay):
		}
	}
}

// cmdProbeInfo is the quiet runner-owned execution that runs command as a
// probe of p: named "<name>:probe" (':' is not allowed in process names, so it
// cannot collide), in p's dir and env. Readiness and health probes share it.
func cmdProbeInfo(p procInfo, command string) procInfo {
	return procInfo{
		name:  p.name + ":probe",
		proc:  config.Process{Cmd: command, Dir: p.proc.Dir, Env: p.proc.Env, Mode: config.ProcessModeTask},
		quiet: true,
	}
}

// monitorHealth runs p's health_when checks for incarnation gen until ctx (the
// incarnation's context) ends or handoff closes. It waits for the incarnation
// to become ready and exits if it never does. Checks are strictly sequential:
// each starts health_interval after the previous result, and is limited to
// min(health_interval, 10s). A check refused by shutdown admission, or one the
// supervisor could not launch, is skipped, not counted. With health_action: restart, the unhealthy transition cancels
// the incarnation; the run loop counts that as a failure (restart policy and
// max_restarts apply). The caller joins it before the incarnation ends.
func (r *Runner) monitorHealth(ctx context.Context, cancel context.CancelFunc, p procInfo, st *procState, gen uint64, handoff <-chan struct{}) {
	cycle := st.currentReadyCycle()
	select {
	case <-cycle.ch:
	case <-ctx.Done():
		return
	case <-handoff:
		return
	}
	st.mu.Lock()
	ready := cycle.outcome == readySatisfied && st.generation == gen
	st.mu.Unlock()
	if !ready {
		return
	}

	hw := p.proc.HealthWhen
	interval := time.Duration(p.proc.EffectiveHealthInterval()) * time.Second
	timeout := min(interval, cmdProbeAttemptTimeout)
	restart := p.proc.EffectiveHealthAction() == config.HealthActionRestart
	timer := time.NewTimer(interval)
	defer timer.Stop()
	refusing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-handoff:
			return
		case <-timer.C:
		}
		ok, detail, skipped, owner := r.healthCheck(ctx, p, hw, timeout, handoff)
		if ctx.Err() != nil {
			return
		}
		if ok && owner != nil {
			refusal := refusalOf(owner, p.proc.EffectiveForeignListener() == config.ForeignListenerReject, r.RefuseCrossSession)
			if st.recordHealthOwner(ctx, gen, owner) {
				r.processLog(p, "warn", "health.foreign_listener", foreignListenerMessage(p.name, "health check", owner, refusal))
			}
			if refusal != "" {
				kind := "tcp"
				if hw.HTTP != "" {
					kind = "http"
				}
				ok, detail = false, kind+": "+foreignListenerError(p.name, owner).Error()
			}
		}
		// A check the supervisor could not launch is logged when refusals
		// begin, not every interval (the detail can name a fresh temp file),
		// and the verdict becomes unknown until a check runs again.
		if skipped && detail != "" && !refusing {
			r.processLog(p, "warn", "health.check_skipped", detail)
			if st.recordHealthSkipped(ctx, gen, detail) {
				r.broadcastState(p, st)
			}
		}
		refusing = skipped && detail != ""
		if !skipped {
			transition := st.recordHealth(ctx, gen, ok, detail, restart)
			if transition == healthCheckFailed || transition == healthBecameUnhealthy {
				r.processLog(p, "debug", "health.check_failed", "health check failed: "+detail)
			}
			switch transition {
			case healthBecameHealthy:
				r.broadcastState(p, st)
			case healthRecovered:
				r.broadcastState(p, st)
				r.processLog(p, "info", "health.recovered", "healthy again")
			case healthBecameUnhealthy:
				r.broadcastState(p, st)
				msg := fmt.Sprintf("unhealthy: %d consecutive health checks failed (last: %s)", healthThreshold, detail)
				if restart {
					msg += "; restarting through the restart policy"
				}
				r.processLog(p, "error", "health.unhealthy", msg)
				if restart {
					cancel()
					return
				}
			}
		}
		timer.Reset(interval)
	}
}

// healthCheck runs one health_when check. skipped reports a check that did
// not run because shutdown refused it, the incarnation ended, or the
// supervisor could not launch it; only in the last case is detail set. A
// passing http or tcp check also returns who answered it, attributed when its
// connection was dialed.
func (r *Runner) healthCheck(ctx context.Context, p procInfo, hw *config.ReadyWhen, timeout time.Duration, handoff <-chan struct{}) (ok bool, detail string, skipped bool, owner *ipc.EndpointOwner) {
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	attribute := func(c probeConn) *ipc.EndpointOwner { return r.attribute(p, c) }
	switch {
	case hw.HTTP != "":
		owner, err := httpOnce(checkCtx, hw.HTTP, attribute)
		if err != nil {
			return false, "http: " + err.Error(), false, nil
		}
		return true, "", false, owner
	case hw.TCP != "":
		owner, err := tcpOnce(checkCtx, &net.Dialer{}, hw.TCP, attribute)
		if err != nil {
			return false, "tcp: " + err.Error(), false, nil
		}
		return true, "", false, owner
	case hw.Cmd != "":
		st := &procState{state: "starting", generation: 1, readyCycle: newReadyCycle(), restartCh: make(chan struct{}, 1)}
		res := r.runOwned(checkCtx, cmdProbeInfo(p, hw.Cmd), st, handoff, cmdProbeOutputLimit)
		switch {
		case exitedZeroInTime(checkCtx, &res):
			return true, "", false, nil
		case res.HandedOff || res.AdmissionRefused || ctx.Err() != nil:
			return false, "", true, nil
		case !res.Started && res.Err != nil:
			// The supervisor could not launch the check (its crash-record
			// entry could not be written, or the shell could not start).
			// That says nothing about the service, and counting it would
			// let health_action: restart kill a healthy service that the
			// same condition then stops from relaunching.
			return false, "health check not run: " + res.Err.Error(), true, nil
		}
		return false, cmdProbeFailure(res), false, nil
	}
	return false, "", true, nil
}

// exitedZeroInTime reports whether a runner-owned execution run under
// attemptCtx (a cmd probe attempt or a teardown process) passed: it exited 0
// before attemptCtx ended. One cut off by its deadline that then exits 0 on
// SIGTERM did not pass. os/exec reports such an exit as the context error only
// when its watcher's cmd.Cancel reaches the group before it is withdrawn, so
// the verdict must not rely on that; the cut is recorded in res.Err so the
// failure detail names it. attemptCtx must not have been cancelled by the
// caller yet.
func exitedZeroInTime(attemptCtx context.Context, res *CheckResult) bool {
	if res.ExitCode == nil || *res.ExitCode != 0 {
		return false
	}
	if err := attemptCtx.Err(); err != nil {
		res.Err = err
		return false
	}
	return true
}

// cmdProbeFailure describes a failed cmd probe attempt: how it ended, then
// the head of its stderr.
func cmdProbeFailure(res CheckResult) string {
	var detail string
	switch {
	case res.Err != nil:
		detail = res.Err.Error()
	case res.Signal != nil:
		detail = "killed by " + *res.Signal
	case res.ExitCode != nil:
		detail = fmt.Sprintf("exit %d", *res.ExitCode)
	default:
		detail = "no exit status"
	}
	if stderr := strings.TrimSpace(res.Stderr); stderr != "" {
		detail += ": " + stderr
	}
	return detail
}

// deadlineFor returns the absolute deadline for a probe, or the zero value
// when timeout <= 0 (meaning "no limit").
func deadlineFor(timeout time.Duration) time.Time {
	if timeout <= 0 {
		return time.Time{}
	}
	return time.Now().Add(timeout)
}
