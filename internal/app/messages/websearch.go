package messages

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/d-kuro/kirocc/internal/anthropic"
	"github.com/d-kuro/kirocc/internal/auth"
	"github.com/d-kuro/kirocc/internal/httpx"
	"github.com/d-kuro/kirocc/internal/kiroclient"
	"github.com/d-kuro/kirocc/internal/respconv"
	"github.com/d-kuro/kirocc/internal/tokencount"
	"github.com/google/uuid"
)

const webSearchQueryPrefix = "Perform a web search for the query:"

// hasNativeWebSearch reports whether any native Anthropic web_search tool is
// present. Claude Code's built-in WebSearch request uses exactly one such tool.
func hasNativeWebSearch(req *anthropic.Request) bool {
	if req == nil {
		return false
	}
	for _, tool := range req.Tools {
		if tool.IsWebSearchTool() {
			return true
		}
	}
	return false
}

func isPureNativeWebSearch(req *anthropic.Request) bool {
	return req != nil && len(req.Tools) == 1 && req.Tools[0].IsWebSearchTool()
}

// extractWebSearchQuery uses the last non-empty user text turn and strips the
// fixed prefix emitted by Claude Code's WebSearch subrequest.
func extractWebSearchQuery(req *anthropic.Request) string {
	if req == nil {
		return ""
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		msg := req.Messages[i]
		if msg.Role != "user" {
			continue
		}
		text := strings.TrimSpace(msg.Content.String())
		if text == "" {
			continue
		}
		if strings.HasPrefix(text, webSearchQueryPrefix) {
			text = strings.TrimSpace(text[len(webSearchQueryPrefix):])
		}
		return text
	}
	return ""
}

func (s *Service) runWebSearch(ctx context.Context, w http.ResponseWriter, req *anthropic.Request, creds *auth.Credentials, responseModel string, contextWindowSize int, short string) {
	if !isPureNativeWebSearch(req) {
		httpx.WriteError(w, http.StatusBadRequest, errTypeInvalidRequest, "native web_search cannot be combined with other tools")
		return
	}
	if s.webSearchClient == nil {
		httpx.WriteError(w, http.StatusNotImplemented, errTypeAPI, "native web_search is unavailable")
		return
	}
	query := extractWebSearchQuery(req)
	if query == "" {
		httpx.WriteError(w, http.StatusBadRequest, errTypeInvalidRequest, "cannot extract web search query from messages")
		return
	}

	inputTokens := estimateWebSearchTokens(req)
	if req.Stream {
		s.runStreamingWebSearch(ctx, w, req, creds, responseModel, contextWindowSize, query, inputTokens, short)
		return
	}

	result, err := s.webSearchClient.SearchWeb(ctx, creds.AccessToken, creds.ProfileARN, creds.Region, query)
	if err != nil {
		logUpstreamError(ctx, short, err, "operation", "web_search")
		final := classifyUpstreamHTTPError(err, "Kiro web search failed")
		httpx.WriteError(w, final.status, final.jsonType, final.jsonMessage)
		return
	}

	toolUseID := "srvtoolu_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	content := buildWebSearchResultContent(result.Results)
	summary := buildWebSearchSummary(query, result.Results)
	outputTokens := estimateTextTokens(summary)
	httpx.WriteJSON(w, http.StatusOK, buildWebSearchResponse(responseModel, toolUseID, query, content, summary, inputTokens, outputTokens))
	logResponseStats(ctx, short, inputTokens, outputTokens, false, 0, contextWindowSize, 0, false)
}

func (s *Service) runStreamingWebSearch(ctx context.Context, w http.ResponseWriter, req *anthropic.Request, creds *auth.Credentials, responseModel string, contextWindowSize int, query string, inputTokens int, short string) {
	session := newStreamSession(ctx, w, s.keepAliveInterval)
	defer session.Stop()

	// Headers and keep-alives stay unsent until the first upstream call
	// succeeds. Writing a 200 with SSE headers first would commit the response,
	// leaving an upstream rejection no way to be reported as its real status.
	result, err := s.webSearchClient.SearchWeb(session.Context(), creds.AccessToken, creds.ProfileARN, creds.Region, query)
	if err != nil {
		logUpstreamError(ctx, short, err, "operation", "web_search")
		_ = session.WriteFinalError(classifyUpstreamHTTPError(err, "Kiro web search failed"), nil)
		return
	}
	session.SetSSEHeaders()
	session.Start()

	toolUseID := "srvtoolu_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	content := buildWebSearchResultContent(result.Results)
	summary := buildWebSearchSummary(query, result.Results)
	outputTokens := estimateTextTokens(summary)
	inputJSON, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		_ = session.WriteFinalError(newStreamFinalError(http.StatusInternalServerError, errTypeAPI, "failed to encode web search response"), nil)
		return
	}

	sw := respconv.NewSSEWriter(session.Context(), session, responseModel, contextWindowSize, req.StopSequences, req.MaxTokens, inputTokens)
	sw.OnVisibleOutput = session.Promote
	sw.WriteServerToolUse(toolUseID, "web_search", string(inputJSON))
	sw.WriteWebSearchResult(toolUseID, content)
	sw.WriteText(summary)
	if err := sw.FinishWebSearch(inputTokens, outputTokens); err != nil {
		slog.DebugContext(ctx, "write web search stream failed", "trace_id", short, "err", err)
		return
	}
	logResponseStats(ctx, short, inputTokens, outputTokens, false, 0, contextWindowSize, 0, false)
}

func buildWebSearchResponse(model, toolUseID, query string, resultContent []map[string]any, summary string, inputTokens, outputTokens int) map[string]any {
	return map[string]any{
		"id":   "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24],
		"type": "message",
		"role": "assistant",
		"content": []any{
			map[string]any{
				"type":  anthropic.BlockTypeServerToolUse,
				"id":    toolUseID,
				"name":  "web_search",
				"input": map[string]string{"query": query},
			},
			map[string]any{
				"type":        anthropic.BlockTypeWebSearchToolResult,
				"tool_use_id": toolUseID,
				"content":     resultContent,
			},
			map[string]any{"type": anthropic.BlockTypeText, "text": summary},
		},
		"model":         model,
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":                inputTokens,
			"output_tokens":               outputTokens,
			"cache_read_input_tokens":     0,
			"cache_creation_input_tokens": 0,
			"server_tool_use": map[string]any{
				"web_search_requests": 1,
			},
		},
	}
}

func buildWebSearchResultContent(results []kiroclient.WebSearchResult) []map[string]any {
	content := make([]map[string]any, 0, len(results))
	for _, result := range results {
		var pageAge any
		if result.PublishedDate != nil && *result.PublishedDate > 0 {
			pageAge = time.UnixMilli(*result.PublishedDate).UTC().Format("January 2, 2006")
		}
		content = append(content, map[string]any{
			"type":              anthropic.BlockTypeWebSearchResult,
			"title":             result.Title,
			"url":               result.URL,
			"encrypted_content": result.Snippet,
			"page_age":          pageAge,
		})
	}
	return content
}

func buildWebSearchSummary(query string, results []kiroclient.WebSearchResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<web_search>\nSearch results for %q:\n\n", query)
	if len(results) == 0 {
		b.WriteString("No results found.\n")
	}
	for i, result := range results {
		fmt.Fprintf(&b, "%d. %s\n", i+1, result.Title)
		if result.URL != "" {
			fmt.Fprintf(&b, "   URL: %s\n", result.URL)
		}
		if result.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", result.Snippet)
		}
		b.WriteByte('\n')
	}
	b.WriteString("</web_search>")
	return b.String()
}

func estimateWebSearchTokens(req *anthropic.Request) int {
	b, err := json.Marshal(req)
	if err != nil {
		return 0
	}
	n, err := tokencount.CountBytes(b)
	if err != nil {
		return 0
	}
	return n
}

func estimateTextTokens(text string) int {
	n, err := tokencount.CountBytes([]byte(text))
	if err != nil {
		if text == "" {
			return 0
		}
		return max(1, len([]rune(text))/4)
	}
	return n
}
