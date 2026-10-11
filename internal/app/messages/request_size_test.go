package messages

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const expectedRequestLimit = 32 << 20

func sizedJSONRequest(size int, padding string) io.Reader {
	const prefix = `{"messages":[{"role":"user","content":"`
	const suffix = `"}]}`
	return io.MultiReader(strings.NewReader(prefix), strings.NewReader(padding[:size-len(prefix)-len(suffix)]), strings.NewReader(suffix))
}

func TestParseRequestSizeBoundary(t *testing.T) {
	padding := strings.Repeat("x", expectedRequestLimit+1)
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	for _, debug := range []bool{false, true} {
		level := slog.LevelInfo
		if debug {
			level = slog.LevelDebug
		}
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: level})))
		for _, size := range []int{(4 << 20) + 128, expectedRequestLimit, expectedRequestLimit + 1} {
			t.Run(fmt.Sprintf("debug=%t/size=%d", debug, size), func(t *testing.T) {
				req := httptest.NewRequest("POST", "/v1/messages", sizedJSONRequest(size, padding))
				_, err := parseAndValidateRequest(t.Context(), httptest.NewRecorder(), req)
				if size <= expectedRequestLimit {
					if err != nil {
						t.Fatalf("valid request rejected: %v", err)
					}
					return
				}
				var tooLarge *http.MaxBytesError
				if !errors.As(err, &tooLarge) {
					t.Fatalf("oversized request error = %v, want MaxBytesError", err)
				}
			})
		}
	}
}

func TestRequestSizeErrorsOnBothEndpoints(t *testing.T) {
	padding := strings.Repeat("x", expectedRequestLimit+1)
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	for _, debug := range []bool{false, true} {
		level := slog.LevelInfo
		if debug {
			level = slog.LevelDebug
		}
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: level})))
		for _, endpoint := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
			for _, knownLength := range []bool{false, true} {
				t.Run(fmt.Sprintf("debug=%t%s/known_length=%t", debug, endpoint, knownLength), func(t *testing.T) {
					service := New(nil, nil) // Any auth/upstream call before rejection is a failure.
					for _, malformed := range []bool{false, true} {
						body := sizedJSONRequest(expectedRequestLimit+1, padding)
						want := http.StatusRequestEntityTooLarge
						if malformed {
							body = strings.NewReader(`{"messages":`)
							want = http.StatusBadRequest
						}
						req := httptest.NewRequest("POST", endpoint, body)
						if knownLength && !malformed {
							req.ContentLength = expectedRequestLimit + 1
						} else {
							req.ContentLength = -1
						}
						rec := httptest.NewRecorder()
						if endpoint == "/v1/messages" {
							service.HandleMessages(rec, req)
						} else {
							service.HandleCountTokens(rec, req)
						}
						if rec.Code != want {
							t.Fatalf("status=%d, want %d; body=%s", rec.Code, want, rec.Body.String())
						}
						if !strings.Contains(rec.Body.String(), `"type":"invalid_request_error"`) {
							t.Fatal("missing Anthropic error envelope")
						}
						if !malformed && !strings.Contains(rec.Body.String(), "32 MiB") {
							t.Fatal("size error does not describe limit")
						}
					}
				})
			}
		}
	}
}
