package kiroclient

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/d-kuro/kirocc/internal/logging"
	"github.com/google/uuid"
)

const maxMCPResponseBytes = 4 << 20

// WebSearchResponse is the expanded JSON payload embedded in Kiro MCP's
// result.content[0].text field.
type WebSearchResponse struct {
	Results      []WebSearchResult `json:"results"`
	TotalResults int               `json:"totalResults"`
	Query        string            `json:"query"`
}

// WebSearchResult is one result returned by Kiro's native web_search tool.
type WebSearchResult struct {
	Title         string `json:"title"`
	URL           string `json:"url"`
	Snippet       string `json:"snippet"`
	PublishedDate *int64 `json:"publishedDate"`
}

type mcpRequest struct {
	ID      string           `json:"id"`
	JSONRPC string           `json:"jsonrpc"`
	Method  string           `json:"method"`
	Params  mcpRequestParams `json:"params"`
}

type mcpRequestParams struct {
	Name      string              `json:"name"`
	Arguments mcpRequestArguments `json:"arguments"`
}

type mcpRequestArguments struct {
	Query string `json:"query"`
}

type mcpResponse struct {
	Result *mcpResult `json:"result"`
	Error  *mcpError  `json:"error"`
}

type mcpResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *HTTPClient) mcpEndpointURL(region string) (string, error) {
	if c.mcpURL != "" {
		return c.mcpURL, nil
	}
	region = strings.TrimSpace(c.effectiveRegion(region))
	if region == "" {
		return "", fmt.Errorf("kiro MCP: region is required")
	}
	return fmt.Sprintf("https://q.%s.amazonaws.com/mcp", region), nil
}

// SearchWeb calls Kiro's MCP tools/call endpoint and expands its nested result
// JSON. The authentication, refresh and transient retry behavior matches the
// regular Kiro runtime client.
func (c *HTTPClient) SearchWeb(ctx context.Context, token, profileARN, region, query string) (*WebSearchResponse, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("kiro MCP: search query is empty")
	}
	endpoint, err := c.mcpEndpointURL(region)
	if err != nil {
		return nil, err
	}

	reqBody, err := json.Marshal(mcpRequest{
		ID:      "web_search_tooluse_" + strings.ReplaceAll(uuid.NewString(), "-", "") + fmt.Sprintf("_%d", time.Now().UnixMilli()),
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params: mcpRequestParams{
			Name:      "web_search",
			Arguments: mcpRequestArguments{Query: query},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal Kiro MCP request: %w", err)
	}

	currentToken := token
	currentProfileARN := profileARN
	credentialTokens := []string{token}
	authRetried := false
	invocationID := uuid.NewString()
	traceID, short := logging.TraceIDs(ctx)

	for attempt := range maxAttempts {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
		if err != nil {
			return nil, fmt.Errorf("create Kiro MCP request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+currentToken)
		if c.apiKeyAuth {
			req.Header.Set("TokenType", "API_KEY")
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "*/*")
		req.Header.Set("User-Agent", userAgentValue)
		req.Header.Set("x-amz-user-agent", amzUserAgentValue)
		req.Header.Set("x-amzn-codewhisperer-optout", "false")
		req.Header.Set("amz-sdk-invocation-id", invocationID)
		req.Header.Set("amz-sdk-request", fmt.Sprintf("attempt=%d; max=%d", attempt+1, maxAttempts))
		if currentProfileARN != "" {
			req.Header.Set("x-amzn-kiro-profile-arn", currentProfileARN)
		}

		slog.DebugContext(ctx, "kiro MCP request headers",
			"trace_id", traceID,
			"session_id", logging.SessionIDFromContext(ctx),
			"headers", logging.SafeHeaders{H: req.Header},
		)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if attempt < maxAttempts-1 {
				delay := backoffDelay(attempt)
				slog.WarnContext(ctx, "kiro MCP: request error, retrying",
					"trace_id", short, "attempt", attempt+1, "max", maxAttempts,
					"delay", delay, "err", err)
				if waitErr := retryWait(ctx, delay); waitErr != nil {
					return nil, waitErr
				}
				continue
			}
			c.recordError(ctx, err)
			return nil, fmt.Errorf("do Kiro MCP request: %w", err)
		}

		resp.Header = safeResponseHeaders(resp.Header, credentialTokens)
		var errBody string
		if resp.StatusCode != http.StatusOK {
			errBody = readSafeErrorBody(resp.Body, credentialTokens)
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			body, readErr := readMCPResponseBody(resp.Body)
			if readErr != nil {
				c.recordError(ctx, readErr)
				return nil, readErr
			}
			result, parseErr := parseMCPWebSearchResponse(body)
			if parseErr != nil {
				parseErr = fmt.Errorf("%s", redactCredentials(parseErr.Error(), credentialTokens))
				c.recordError(ctx, parseErr)
				return nil, parseErr
			}
			return result, nil

		case resp.StatusCode == http.StatusForbidden:
			// Same bounded single retry as the runtime client: bearer and
			// profile ARN move together, and an unchanged snapshot is not worth
			// a second request.
			if !c.apiKeyAuth && !authRetried && attempt < maxAttempts-1 && c.tokenRefresher != nil {
				authRetried = true
				refreshed, refreshErr := c.tokenRefresher(ctx)
				nextToken, nextProfileARN := refreshed.resolve(currentToken, currentProfileARN)
				switch {
				case refreshErr != nil:
					slog.WarnContext(ctx, "kiro MCP: token refresh failed", "trace_id", short, "err", redactCredentials(refreshErr.Error(), credentialTokens))
				case nextToken == currentToken && nextProfileARN == currentProfileARN:
					slog.InfoContext(ctx, "kiro MCP: 403 received, credential snapshot unchanged",
						"trace_id", short, "attempt", attempt+1, "max", maxAttempts)
				default:
					currentToken, currentProfileARN = nextToken, nextProfileARN
					credentialTokens = append(credentialTokens, nextToken)
					slog.InfoContext(ctx, "kiro MCP: 403 received, credential snapshot refreshed",
						"trace_id", short, "attempt", attempt+1, "max", maxAttempts)
					continue
				}
			}

		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			if attempt < maxAttempts-1 {
				delay := backoffDelay(attempt)
				slog.WarnContext(ctx, "kiro MCP: upstream error, retrying",
					"trace_id", short, "status", resp.StatusCode,
					"attempt", attempt+1, "max", maxAttempts, "delay", delay)
				if waitErr := retryWait(ctx, delay); waitErr != nil {
					return nil, waitErr
				}
				continue
			}
		}

		ue := &UpstreamError{
			Status:      resp.StatusCode,
			ContentType: resp.Header.Get("Content-Type"),
			Exception:   resolveAWSException(errBody, resp.Header),
			Body:        errBody,
		}
		c.recordError(ctx, ue)
		return nil, ue
	}

	return nil, fmt.Errorf("kiro MCP: max retries exceeded")
}

func readMCPResponseBody(body io.ReadCloser) ([]byte, error) {
	defer func() { _ = body.Close() }()
	limited := io.LimitReader(body, maxMCPResponseBytes+1)
	b, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read Kiro MCP response: %w", err)
	}
	if len(b) > maxMCPResponseBytes {
		return nil, fmt.Errorf("kiro MCP response exceeds %d bytes", maxMCPResponseBytes)
	}
	return b, nil
}

func parseMCPWebSearchResponse(body []byte) (*WebSearchResponse, error) {
	var rpc mcpResponse
	if err := json.Unmarshal(body, &rpc); err != nil {
		return nil, fmt.Errorf("parse Kiro MCP response: %w", err)
	}
	if rpc.Error != nil {
		return nil, fmt.Errorf("kiro MCP error %d: %s", rpc.Error.Code, rpc.Error.Message)
	}
	if rpc.Result == nil {
		return nil, fmt.Errorf("kiro MCP response has no result")
	}
	if rpc.Result.IsError {
		return nil, fmt.Errorf("kiro MCP web_search returned a tool error")
	}
	if len(rpc.Result.Content) == 0 || rpc.Result.Content[0].Type != "text" {
		return nil, fmt.Errorf("kiro MCP web_search result has no text content")
	}
	var result WebSearchResponse
	if err := json.Unmarshal([]byte(rpc.Result.Content[0].Text), &result); err != nil {
		return nil, fmt.Errorf("parse Kiro MCP embedded search results: %w", err)
	}
	return &result, nil
}
