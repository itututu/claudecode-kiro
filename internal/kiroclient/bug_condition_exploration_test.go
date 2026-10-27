package kiroclient

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/d-kuro/kirocc/internal/kiroproto"
)

// TestProperty1BugConditionExplorationAuthorization exercises scoped fake
// upstream authorization failures with synthetic credentials only.
//
// **Validates: Requirements 1.1–1.20, 2.1–2.20**
func TestProperty1BugConditionExplorationAuthorization(t *testing.T) {
	t.Run("API key 403 is one attempt without refresh", func(t *testing.T) {
		var requests, refreshes atomic.Int32
		server := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusForbidden)
		}))
		defer server.Close()

		client := NewHTTPClient(
			WithBaseURL(server.URL),
			WithAPIKeyAuth(),
			WithTokenRefresher(func(context.Context) (RefreshedCredentials, error) {
				refreshes.Add(1)
				return RefreshedCredentials{AccessToken: "ksk_synthetic-refreshed"}, nil
			}),
		)
		_, _ = client.GenerateAssistantResponse(t.Context(), "ksk_synthetic-original", bugConditionPayload(""), "us-east-1")
		if requests.Load() != 1 || refreshes.Load() != 0 {
			t.Errorf("counterexample: auth_mode=api_key status=403 requests=%d refreshes=%d", requests.Load(), refreshes.Load())
		}
	})

	t.Run("unchanged OAuth snapshot is not retried", func(t *testing.T) {
		var requests, refreshes atomic.Int32
		server := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusForbidden)
		}))
		defer server.Close()

		client := NewHTTPClient(
			WithBaseURL(server.URL),
			WithTokenRefresher(func(context.Context) (RefreshedCredentials, error) {
				refreshes.Add(1)
				return RefreshedCredentials{
					AccessToken: "synthetic-oauth-original",
					ProfileARN:  "arn:synthetic:profile/original",
				}, nil
			}),
		)
		_, _ = client.GenerateAssistantResponse(t.Context(), "synthetic-oauth-original", bugConditionPayload("arn:synthetic:profile/original"), "us-east-1")
		if requests.Load() != 1 || refreshes.Load() != 1 {
			t.Errorf("counterexample: auth_mode=oauth snapshot_changed=false requests=%d refreshes=%d", requests.Load(), refreshes.Load())
		}
	})

	t.Run("OAuth retry changes token and profile atomically", func(t *testing.T) {
		var requests, refreshes atomic.Int32
		var secondBearerChanged, secondProfileChanged atomic.Bool
		server := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestNumber := requests.Add(1)
			var body struct {
				ProfileARN string `json:"profileArn"`
			}
			_ = json.UnmarshalRead(r.Body, &body)
			if requestNumber == 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			secondBearerChanged.Store(r.Header.Get("Authorization") == "Bearer synthetic-oauth-refreshed")
			secondProfileChanged.Store(body.ProfileARN == "arn:synthetic:profile/refreshed")
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		client := NewHTTPClient(
			WithBaseURL(server.URL),
			WithTokenRefresher(func(context.Context) (RefreshedCredentials, error) {
				refreshes.Add(1)
				return RefreshedCredentials{
					AccessToken: "synthetic-oauth-refreshed",
					ProfileARN:  "arn:synthetic:profile/refreshed",
				}, nil
			}),
		)
		response, _ := client.GenerateAssistantResponse(t.Context(), "synthetic-oauth-original", bugConditionPayload("arn:synthetic:profile/original"), "us-east-1")
		if response != nil {
			_ = response.Body.Close()
		}
		if requests.Load() != 2 || refreshes.Load() != 1 || !secondBearerChanged.Load() || !secondProfileChanged.Load() {
			t.Errorf("counterexample: auth_mode=oauth requests=%d refreshes=%d bearer_changed=%t profile_changed=%t", requests.Load(), refreshes.Load(), secondBearerChanged.Load(), secondProfileChanged.Load())
		}
	})
}

func bugConditionPayload(profileARN string) *kiroproto.Payload {
	return &kiroproto.Payload{
		ProfileARN: profileARN,
		ConversationState: kiroproto.ConversationState{
			AgentTaskType:   "vibe",
			ChatTriggerType: "MANUAL",
			CurrentMessage: kiroproto.CurrentMessage{
				UserInputMessage: kiroproto.UserInputMessage{Content: "synthetic"},
			},
		},
	}
}
