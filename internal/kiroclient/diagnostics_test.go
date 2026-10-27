package kiroclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAPIKeyForbiddenPreservesSafeDiagnosticsWithoutRefresh(t *testing.T) {
	for _, endpoint := range []string{"runtime", "web search"} {
		t.Run(endpoint, func(t *testing.T) {
			const token = "ksk_private-test-credential"
			var requests, refreshes atomic.Int32
			srv := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/x-amz-json-1.0; diagnostic="+token)
				w.Header().Set("X-Amzn-ErrorType", "AccessDeniedException:"+token)
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"message":"credential rejected: Bearer ` + token + `"}`))
			}))
			defer srv.Close()
			var logs bytes.Buffer
			previousLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			defer slog.SetDefault(previousLogger)

			c := NewHTTPClient(WithBaseURL(srv.URL), WithMCPURL(srv.URL), WithAPIKeyAuth(),
				WithTokenRefresher(func(context.Context) (RefreshedCredentials, error) {
					refreshes.Add(1)
					return RefreshedCredentials{AccessToken: token}, nil
				}))
			var err error
			if endpoint == "runtime" {
				_, err = c.GenerateAssistantResponse(t.Context(), token, apiKeyTestPayload(), "us-east-1")
			} else {
				_, err = c.SearchWeb(t.Context(), token, "", "us-east-1", "q")
			}
			var upstream *UpstreamError
			if !errors.As(err, &upstream) {
				t.Fatalf("error type = %T, want *UpstreamError", err)
			}
			if requests.Load() != 1 || refreshes.Load() != 0 {
				t.Fatalf("requests=%d refreshes=%d, want 1 and 0", requests.Load(), refreshes.Load())
			}
			if upstream.Status != http.StatusForbidden || upstream.Exception != "AccessDeniedException" {
				t.Fatalf("wrong upstream diagnostics: status=%d exception=%q", upstream.Status, upstream.Exception)
			}
			if !strings.Contains(upstream.Body, "credential rejected: Bearer [REDACTED]") {
				t.Fatal("403 body lost its redacted upstream message")
			}
			if strings.Contains(err.Error(), token) || strings.Contains(logs.String(), token) {
				t.Fatal("credential leaked into errors or logs")
			}
			if !strings.Contains(logs.String(), "[REDACTED]") {
				t.Fatal("request Authorization header was not logged as redacted")
			}
		})
	}
}

func TestForbiddenAfterOAuthRetriesRedactsAllTokens(t *testing.T) {
	for _, endpoint := range []string{"runtime", "web search"} {
		t.Run(endpoint, func(t *testing.T) {
			const original, refreshed = "original-secret", "refreshed-secret"
			var requests, refreshes atomic.Int32
			srv := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/x-amz-json-1.0")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"__type":"AccessDeniedException","message":"rejected ` + original + ` and ` + refreshed + `"}`))
			}))
			defer srv.Close()
			c := NewHTTPClient(WithBaseURL(srv.URL), WithMCPURL(srv.URL),
				WithTokenRefresher(func(context.Context) (RefreshedCredentials, error) {
					refreshes.Add(1)
					return RefreshedCredentials{AccessToken: refreshed}, nil
				}))
			var err error
			if endpoint == "runtime" {
				_, err = c.GenerateAssistantResponse(t.Context(), original, apiKeyTestPayload(), "us-east-1")
			} else {
				_, err = c.SearchWeb(t.Context(), original, "", "us-east-1", "q")
			}
			var upstream *UpstreamError
			if !errors.As(err, &upstream) {
				t.Fatalf("error type = %T, want *UpstreamError", err)
			}
			// One auth retry, not the whole attempt budget: the second 403 is
			// the authorization answer, and both credentials must still be
			// redacted out of the diagnostics that answer carries.
			if requests.Load() != 2 || refreshes.Load() != 1 {
				t.Fatalf("requests=%d refreshes=%d, want 2 and 1", requests.Load(), refreshes.Load())
			}
			if upstream.Exception != "AccessDeniedException" || !strings.Contains(upstream.Body, "rejected [REDACTED] and [REDACTED]") {
				t.Fatal("final OAuth rejection lost safe body/exception diagnostics")
			}
		})
	}
}

func TestReadSafeErrorBodyBoundsAndRedactsBoundary(t *testing.T) {
	const token = "ksk_boundary-private-credential"
	// Include enough earlier credentials that redaction shortens the output.
	// The bounded read must still not retain a partial credential at its end.
	prefix := strings.Repeat(token+" ", 20)
	prefix += strings.Repeat(".", upstreamBodyLimit-len(prefix)-len("ksk_boundary"))
	body := prefix + token + " remainder"
	got := readSafeErrorBody(io.NopCloser(strings.NewReader(body)), []string{token})
	if len(got) > upstreamBodyLimit {
		t.Fatalf("body length = %d, exceeds diagnostic limit", len(got))
	}
	if strings.Contains(got, "ksk_") || !strings.HasSuffix(got, "[REDACTED]") {
		t.Fatal("bounded diagnostic contains a partial credential")
	}
}

func TestCredentialRedactionIncludesEncodedForms(t *testing.T) {
	const token = `key+/"secret`
	got := redactCredentials(`raw key+/"secret json key+/\"secret url key%2B%2F%22secret`, []string{token})
	if got != "raw [REDACTED] json [REDACTED] url [REDACTED]" {
		t.Fatal("raw, JSON, or URL-encoded credential was not redacted")
	}
}
