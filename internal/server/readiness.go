package server

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"sync"
	"time"

	"github.com/d-kuro/kirocc/internal/httpx"
)

// ProbeState is the process-local upstream readiness state. It starts at
// ProbeUnattempted and is decided exactly once per gateway process: a restart
// starts a brand new tracker, and nothing outside a marked readiness probe can
// move it.
type ProbeState string

const (
	ProbeUnattempted ProbeState = "unattempted"
	ProbeInProgress  ProbeState = "in_progress"
	ProbeSucceeded   ProbeState = "succeeded"
	ProbeFailed      ProbeState = "failed"
)

// ReadinessHeader marks a /v1/messages request as the launcher's readiness
// probe. The value is a fixed non-secret marker; any other request is served
// without touching readiness state.
const ReadinessHeader = "X-Kirocc-Readiness-Probe"

// readinessBodyLimit bounds how much of a probe response is buffered to decide
// success. A valid Anthropic message envelope is far smaller than this; the cap
// exists so a runaway upstream cannot be mirrored into memory.
const readinessBodyLimit = 1 << 20

// readinessTracker holds the exactly-once readiness decision for this process.
//
// Invariants, enforced on every transition:
//   - upstream_ready is true if and only if state == ProbeSucceeded
//   - readyAt is non-zero only in ProbeSucceeded
type readinessTracker struct {
	mu      sync.Mutex
	state   ProbeState
	readyAt time.Time
}

func newReadinessTracker() *readinessTracker {
	return &readinessTracker{state: ProbeUnattempted}
}

// TryBeginProbe moves ProbeUnattempted to ProbeInProgress and reports whether
// this caller won. Only the winner may run the probe; every later marked
// request is rejected rather than queued, so a second launcher invocation can
// never spend another upstream inference.
func (t *readinessTracker) TryBeginProbe() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != ProbeUnattempted {
		return false
	}
	t.state = ProbeInProgress
	return true
}

// CompleteProbe records the outcome of the winning probe. Success means the
// Kiro event stream was converted into a valid local Anthropic message, not
// merely that the request returned.
func (t *readinessTracker) CompleteProbe(success bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if success {
		t.state = ProbeSucceeded
		t.readyAt = time.Now().UTC()
		return
	}
	t.state = ProbeFailed
	t.readyAt = time.Time{}
}

// Snapshot returns the current state and, only when succeeded, the instant
// readiness was established.
func (t *readinessTracker) Snapshot() (ProbeState, time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state, t.readyAt
}

// readinessProbeMiddleware wraps the /v1/messages handler. Unmarked requests
// pass straight through and can never change readiness state.
func (s *Server) readinessProbeMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(ReadinessHeader) == "" {
			next(w, r)
			return
		}
		if !s.readiness.TryBeginProbe() {
			state, _ := s.readiness.Snapshot()
			// Stable, authenticated, non-secret: the caller learns only which
			// state already decided this process.
			httpx.WriteError(w, http.StatusConflict, httpx.ErrTypeInvalidRequest,
				"readiness probe already "+string(state))
			return
		}
		rec := &probeRecorder{ResponseWriter: w, status: http.StatusOK}
		// Deferred so a panic or an early return inside the handler still
		// decides the state: leaving it in_progress would strand the process
		// in a state no later probe can repair.
		defer func() { s.readiness.CompleteProbe(rec.validAnthropicMessage()) }()
		next(rec, r)
	}
}

// probeRecorder forwards the probe response to the client while buffering a
// bounded copy so the readiness decision can be made from what was actually
// sent.
type probeRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (p *probeRecorder) WriteHeader(status int) {
	p.status = status
	p.ResponseWriter.WriteHeader(status)
}

func (p *probeRecorder) Write(b []byte) (int, error) {
	if remaining := readinessBodyLimit - p.body.Len(); remaining > 0 {
		if len(b) <= remaining {
			p.body.Write(b)
		} else {
			p.body.Write(b[:remaining])
		}
	}
	return p.ResponseWriter.Write(b)
}

// Flush preserves streaming behavior for a probe that unexpectedly streams.
func (p *probeRecorder) Flush() {
	if f, ok := p.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// validAnthropicMessage reports whether the recorded response is a complete
// Anthropic message. A 200 carrying an error envelope, a truncated body, or a
// `content` that is not an array all count as failure.
func (p *probeRecorder) validAnthropicMessage() bool {
	if p.status != http.StatusOK {
		return false
	}
	var msg struct {
		ID      string         `json:"id"`
		Type    string         `json:"type"`
		Role    string         `json:"role"`
		Content jsontext.Value `json:"content"`
	}
	if err := json.Unmarshal(p.body.Bytes(), &msg); err != nil {
		return false
	}
	if msg.ID == "" || msg.Type != "message" || msg.Role != "assistant" {
		return false
	}
	trimmed := bytes.TrimSpace(msg.Content)
	return len(trimmed) > 0 && trimmed[0] == '['
}
