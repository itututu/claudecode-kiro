package server

import (
	"net/http"

	"github.com/d-kuro/kirocc/internal/httpx"
	"github.com/d-kuro/kirocc/internal/models"
)

// StatusProtocolVersion is the shape version of the /v1/kirocc/status payload.
// A managed launcher compares it before reusing a gateway: an older or newer
// process is not reused rather than being probed for compatibility.
const StatusProtocolVersion = 2

// handleStatus answers the authenticated identity probe a managed launcher
// uses to decide whether an already-running gateway is the one it wants.
//
// The payload is deliberately limited to non-secret configuration identity and
// readiness: no PID, no filesystem path, no credential, no credential
// fingerprint, no profile, no prompt, no request, and no model response. Those
// would turn a reuse check into a credential disclosure channel.
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	state, readyAt := s.readiness.Snapshot()

	// Invariant: ready only in succeeded, and the timestamp exists only there.
	var readyAtValue any
	if state == ProbeSucceeded {
		readyAtValue = readyAt.Format("2006-01-02T15:04:05Z")
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"service":              "kirocc",
		"protocol_version":     StatusProtocolVersion,
		"auth_type":            s.authType,
		"region":               s.region,
		"inference_host":       s.inferenceHost,
		"model_mapping_mode":   models.EffectiveMappingMode(),
		"upstream_probe_state": string(state),
		"upstream_ready":       state == ProbeSucceeded,
		"upstream_ready_at":    readyAtValue,
	})
}
