package messages

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/d-kuro/kirocc/internal/auth"
	"github.com/d-kuro/kirocc/internal/kiroclient"
	"github.com/d-kuro/kirocc/internal/kiroproto"
)

type bugConditionAuth struct{}

func (bugConditionAuth) GetToken(context.Context) (*auth.Credentials, error) {
	return &auth.Credentials{
		AccessToken: "synthetic-token",
		ProfileARN:  "arn:synthetic:profile",
		Region:      "us-east-1",
	}, nil
}

type bugConditionForbiddenClient struct {
	inferenceCalls int
	webSearchCalls int
}

func (c *bugConditionForbiddenClient) GenerateAssistantResponse(context.Context, string, *kiroproto.Payload, string) (*kiroclient.Response, error) {
	c.inferenceCalls++
	return nil, fmt.Errorf("wrapped synthetic rejection: %w", &kiroclient.UpstreamError{Status: http.StatusForbidden})
}

func (c *bugConditionForbiddenClient) SearchWeb(context.Context, string, string, string, string) (*kiroclient.WebSearchResponse, error) {
	c.webSearchCalls++
	return nil, fmt.Errorf("wrapped synthetic rejection: %w", &kiroclient.UpstreamError{Status: http.StatusForbidden})
}

// TestProperty1BugConditionExplorationPermissionErrors verifies the stable
// Claude-compatible authorization boundary for inference and native WebSearch.
//
// **Validates: Requirements 1.1–1.20, 2.1–2.20**
func TestProperty1BugConditionExplorationPermissionErrors(t *testing.T) {
	cases := []struct {
		name      string
		stream    bool
		webSearch bool
	}{
		{name: "inference nonstreaming", stream: false},
		{name: "inference streaming before commitment", stream: true},
		{name: "WebSearch nonstreaming", webSearch: true, stream: false},
		{name: "WebSearch streaming before commitment", webSearch: true, stream: true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client := &bugConditionForbiddenClient{}
			service := New(bugConditionAuth{}, client)
			recorder := httptest.NewRecorder()
			request := bugConditionMessageRequest(testCase.stream, testCase.webSearch)
			service.HandleMessages(recorder, request)

			inferenceCalls, webSearchCalls := client.inferenceCalls, client.webSearchCalls
			if recorder.Code != http.StatusForbidden || permissionErrorType(recorder.Body.Bytes()) != "permission_error" {
				t.Errorf("counterexample: endpoint_websearch=%t stream=%t status=%d error_type=%q inference_requests=%d websearch_requests=%d", testCase.webSearch, testCase.stream, recorder.Code, permissionErrorType(recorder.Body.Bytes()), inferenceCalls, webSearchCalls)
			}
			if inferenceCalls+webSearchCalls != 1 {
				t.Errorf("request_count=%d, want 1", inferenceCalls+webSearchCalls)
			}
		})
	}
}

func bugConditionMessageRequest(stream, webSearch bool) *http.Request {
	tools := "[]"
	if webSearch {
		tools = `[{"max_uses":1,"type":"web_search_20250305","name":"web_search"}]`
	}
	body := fmt.Sprintf(`{"model":"claude-sonnet-4-6","max_tokens":1,"stream":%t,"messages":[{"role":"user","content":"synthetic"}],"tools":%s}`, stream, tools)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set(headerCCSessionID, "synthetic-session")
	return request
}

func permissionErrorType(body []byte) string {
	var response struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "non-json"
	}
	return response.Error.Type
}
