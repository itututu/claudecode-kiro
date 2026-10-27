package server

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/d-kuro/kirocc/internal/auth"
	"github.com/d-kuro/kirocc/internal/kiroclient"
	"github.com/d-kuro/kirocc/internal/kiroproto"
)

const testLocalKey = "kiro-local"

// newIdentityServer builds a server carrying a known upstream identity so the
// status payload can be asserted field by field.
func newIdentityServer(t *testing.T, client kiroclient.Client) *httptest.Server {
	t.Helper()
	mgr := &mockAuthManager{
		creds: &auth.Credentials{AccessToken: "test-token", ProfileARN: "arn:test", Region: "us-east-1"},
	}
	if client == nil {
		client = &mockKiroClient{
			handler: func(context.Context, string, *kiroproto.Payload, string) (*kiroclient.Response, error) {
				p, _ := json.Marshal(map[string]string{"content": "OK"})
				return &kiroclient.Response{StatusCode: 200, Body: buildEventStream("assistantResponseEvent", p), Header: http.Header{}}, nil
			},
		}
	}
	s := New(mgr, testLocalKey, client,
		WithUpstreamIdentity(auth.AuthTypeAPIKey, "us-east-1", "runtime.us-east-1.kiro.dev"))
	return newTCP4TestServer(t, s.Handler())
}

func getStatus(t *testing.T, srv *httptest.Server, bearer string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/kirocc/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("status body is not JSON: %v (%s)", err, body)
	}
	return resp.StatusCode, payload
}

// postProbeMessages sends a minimal non-streaming request, optionally marked as the
// readiness probe.
func postProbeMessages(t *testing.T, srv *httptest.Server, probe bool) int {
	t.Helper()
	body := `{"model":"claude-sonnet-4.6","max_tokens":100,"stream":false,` +
		`"messages":[{"role":"user","content":"Reply exactly OK"}]}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testLocalKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("X-Claude-Code-Session-Id", "test-readiness")
	if probe {
		req.Header.Set(ReadinessHeader, "1")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestStatusRequiresLocalBearer(t *testing.T) {
	srv := newIdentityServer(t, nil)
	defer srv.Close()

	if code, _ := getStatus(t, srv, ""); code != http.StatusUnauthorized {
		t.Errorf("missing bearer: got %d, want 401", code)
	}
	if code, _ := getStatus(t, srv, "wrong-key"); code != http.StatusUnauthorized {
		t.Errorf("wrong bearer: got %d, want 401", code)
	}
}

func TestStatusReportsIdentityAndNoSecrets(t *testing.T) {
	srv := newIdentityServer(t, nil)
	defer srv.Close()

	code, payload := getStatus(t, srv, testLocalKey)
	if code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", code)
	}

	want := map[string]any{
		"service":              "kirocc",
		"protocol_version":     float64(StatusProtocolVersion),
		"auth_type":            auth.AuthTypeAPIKey,
		"region":               "us-east-1",
		"inference_host":       "runtime.us-east-1.kiro.dev",
		"model_mapping_mode":   "builtin",
		"upstream_probe_state": string(ProbeUnattempted),
		"upstream_ready":       false,
		"upstream_ready_at":    nil,
	}
	for k, v := range want {
		if got := payload[k]; got != v {
			t.Errorf("%s: got %#v, want %#v", k, got, v)
		}
	}
	// A reuse check must not become a disclosure channel: anything beyond the
	// agreed identity fields is a leak, however harmless it looks.
	for k := range payload {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected field in status payload: %q", k)
		}
	}
}

func TestReadinessProbeSucceedsOnceAndRejectsSecond(t *testing.T) {
	srv := newIdentityServer(t, nil)
	defer srv.Close()

	if code := postProbeMessages(t, srv, true); code != http.StatusOK {
		t.Fatalf("probe: got %d, want 200", code)
	}
	_, payload := getStatus(t, srv, testLocalKey)
	if payload["upstream_probe_state"] != string(ProbeSucceeded) {
		t.Errorf("state: got %v, want succeeded", payload["upstream_probe_state"])
	}
	if payload["upstream_ready"] != true {
		t.Errorf("upstream_ready: got %v, want true", payload["upstream_ready"])
	}
	if payload["upstream_ready_at"] == nil {
		t.Error("upstream_ready_at must be set once succeeded")
	}

	// A second launcher must not spend another upstream inference.
	if code := postProbeMessages(t, srv, true); code != http.StatusConflict {
		t.Errorf("second probe: got %d, want 409", code)
	}
	_, after := getStatus(t, srv, testLocalKey)
	if after["upstream_ready_at"] != payload["upstream_ready_at"] {
		t.Error("a rejected probe must not move upstream_ready_at")
	}
}

func TestReadinessFailsWhenUpstreamRejects(t *testing.T) {
	client := &mockKiroClient{
		handler: func(context.Context, string, *kiroproto.Payload, string) (*kiroclient.Response, error) {
			return nil, &kiroclient.UpstreamError{Status: http.StatusForbidden}
		},
	}
	srv := newIdentityServer(t, client)
	defer srv.Close()

	if code := postProbeMessages(t, srv, true); code == http.StatusOK {
		t.Fatal("probe should not have returned 200 on a rejected upstream")
	}
	_, payload := getStatus(t, srv, testLocalKey)
	if payload["upstream_probe_state"] != string(ProbeFailed) {
		t.Errorf("state: got %v, want failed", payload["upstream_probe_state"])
	}
	if payload["upstream_ready"] != false {
		t.Errorf("upstream_ready: got %v, want false", payload["upstream_ready"])
	}
	if payload["upstream_ready_at"] != nil {
		t.Error("upstream_ready_at must stay null unless succeeded")
	}
}

func TestUnmarkedTrafficNeverEstablishesReadiness(t *testing.T) {
	srv := newIdentityServer(t, nil)
	defer srv.Close()

	// Real traffic, a liveness check and a static listing must all leave the
	// decision to the launcher's own marked probe.
	if code := postProbeMessages(t, srv, false); code != http.StatusOK {
		t.Fatalf("plain request: got %d, want 200", code)
	}
	for _, path := range []string{"/health", "/v1/models"} {
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+testLocalKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	_, payload := getStatus(t, srv, testLocalKey)
	if payload["upstream_probe_state"] != string(ProbeUnattempted) {
		t.Errorf("state: got %v, want unattempted", payload["upstream_probe_state"])
	}

	// And the launcher's probe still works afterwards.
	if code := postProbeMessages(t, srv, true); code != http.StatusOK {
		t.Fatalf("probe after plain traffic: got %d, want 200", code)
	}
	_, after := getStatus(t, srv, testLocalKey)
	if after["upstream_ready"] != true {
		t.Errorf("upstream_ready: got %v, want true", after["upstream_ready"])
	}
}

func TestProbeRecorderRejectsIncompleteMessages(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"valid", 200, `{"id":"msg_1","type":"message","role":"assistant","content":[]}`, true},
		{"non-200", 403, `{"id":"msg_1","type":"message","role":"assistant","content":[]}`, false},
		{"error envelope", 200, `{"type":"error","error":{"type":"permission_error"}}`, false},
		{"empty id", 200, `{"id":"","type":"message","role":"assistant","content":[]}`, false},
		{"wrong role", 200, `{"id":"msg_1","type":"message","role":"user","content":[]}`, false},
		{"content not array", 200, `{"id":"msg_1","type":"message","role":"assistant","content":"hi"}`, false},
		{"truncated", 200, `{"id":"msg_1","type":"message","role":"assi`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &probeRecorder{ResponseWriter: httptest.NewRecorder(), status: tc.status}
			rec.body.WriteString(tc.body)
			if got := rec.validAnthropicMessage(); got != tc.want {
				t.Errorf("validAnthropicMessage() = %v, want %v", got, tc.want)
			}
		})
	}
}
