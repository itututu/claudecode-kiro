package kiroclient

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHTTPClient_SearchWebContract(t *testing.T) {
	published := int64(1732299319000)
	srv := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			t.Errorf("path = %q, want /mcp", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("x-amzn-kiro-profile-arn"); got != "arn:aws:codewhisperer:us-east-1:1:profile/test" {
			t.Errorf("profile header = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		if got := r.Header.Get("X-Amz-Target"); got != "" {
			t.Errorf("MCP request must not carry X-Amz-Target, got %q", got)
		}

		var body struct {
			JSONRPC string `json:"jsonrpc"`
			Method  string `json:"method"`
			Params  struct {
				Name      string `json:"name"`
				Arguments struct {
					Query string `json:"query"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Fatal(err)
		}
		if body.JSONRPC != "2.0" || body.Method != "tools/call" || body.Params.Name != "web_search" || body.Params.Arguments.Query != "latest Kiro news" {
			t.Fatalf("unexpected MCP request: %+v", body)
		}

		inner := `{"results":[{"title":"Kiro","url":"https://kiro.dev","snippet":"Latest news","publishedDate":1732299319000}],"totalResults":1,"query":"latest Kiro news"}`
		_ = json.MarshalWrite(w, map[string]any{
			"jsonrpc": "2.0",
			"id":      "request-id",
			"result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": inner}},
				"isError": false,
			},
		})
	}))
	defer srv.Close()

	c := NewHTTPClient(WithMCPURL(srv.URL + "/mcp"))
	result, err := c.SearchWeb(t.Context(), "test-token", "arn:aws:codewhisperer:us-east-1:1:profile/test", "us-east-1", "latest Kiro news")
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalResults != 1 || result.Query != "latest Kiro news" || len(result.Results) != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	got := result.Results[0]
	if got.Title != "Kiro" || got.URL != "https://kiro.dev" || got.Snippet != "Latest news" {
		t.Fatalf("unexpected search item: %+v", got)
	}
	if got.PublishedDate == nil || *got.PublishedDate != published {
		t.Fatalf("PublishedDate = %v, want %d", got.PublishedDate, published)
	}
}

func TestHTTPClient_MCPEndpointUsesRegionOverride(t *testing.T) {
	c := NewHTTPClient(WithRegion("us-east-1"))
	got, err := c.mcpEndpointURL("ap-southeast-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://q.us-east-1.amazonaws.com/mcp"; got != want {
		t.Fatalf("mcpEndpointURL = %q, want %q", got, want)
	}
}

func TestHTTPClient_SearchWebRefreshes403(t *testing.T) {
	var calls atomic.Int32
	srv := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer refreshed-token" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"{\"results\":[],\"totalResults\":0,\"query\":\"q\"}"}],"isError":false}}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(
		WithMCPURL(srv.URL),
		WithTokenRefresher(func(_ context.Context) (RefreshedCredentials, error) {
			return RefreshedCredentials{AccessToken: "refreshed-token"}, nil
		}),
	)
	if _, err := c.SearchWeb(t.Context(), "expired-token", "arn:test", "us-east-1", "q"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestHTTPClient_SearchWebRejectsMCPError(t *testing.T) {
	srv := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32602,"message":"bad query"}}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(WithMCPURL(srv.URL))
	_, err := c.SearchWeb(t.Context(), "token", "arn:test", "us-east-1", "q")
	if err == nil || !strings.Contains(err.Error(), "bad query") {
		t.Fatalf("error = %v, want MCP error", err)
	}
}
