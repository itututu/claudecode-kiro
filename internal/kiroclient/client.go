package kiroclient

// Modified by ClaudeCode Kiro to expose native Kiro MCP WebSearch.

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/d-kuro/kirocc/internal/kiroproto"
	"github.com/d-kuro/kirocc/internal/logging"
	"github.com/d-kuro/kirocc/internal/tracing"
	"github.com/google/uuid"
)

const (
	amzTarget = "AmazonCodeWhispererStreamingService.GenerateAssistantResponse"
	// maxAttempts is the total number of request attempts (initial + retries).
	// kiro-cli advertises this verbatim in the amz-sdk-request header
	// (attempt=N; max=3, still observed in the 2.10.0 capture), so the loop
	// count and the header stay consistent.
	maxAttempts    = 3
	baseRetryDelay = 1 * time.Second
)

// User-Agent version components, pinned to the kiro-cli release we emulate.
// Bump these together when targeting a new kiro-cli version; the
// TestUserAgent_Documents2100 drift guard fails if the assembled strings change.
const (
	appVersion              = "2.10.0"
	awsSDKRustVersion       = "1.3.15"
	codewhispererAPIVersion = "0.1.17593"
)

var (
	userAgentValue    = fmt.Sprintf("aws-sdk-rust/%s ua/2.1 api/codewhispererstreaming/%s os/macos lang/rust/1.92.0 md/appVersion-%s app/AmazonQ-For-CLI", awsSDKRustVersion, codewhispererAPIVersion, appVersion)
	amzUserAgentValue = fmt.Sprintf("aws-sdk-rust/%s ua/2.1 api/codewhispererstreaming/%s os/macos lang/rust/1.92.0 m/F app/AmazonQ-For-CLI", awsSDKRustVersion, codewhispererAPIVersion)
)

// Client is the interface for calling the Kiro API.
type Client interface {
	GenerateAssistantResponse(ctx context.Context, token string, payload *kiroproto.Payload, region string) (*Response, error)
}

// WebSearchClient is the optional Kiro MCP capability used for Anthropic's
// native web_search server tool. Keeping it separate preserves compatibility
// with existing Client implementations and test doubles.
type WebSearchClient interface {
	SearchWeb(ctx context.Context, token, profileARN, region, query string) (*WebSearchResponse, error)
}

// Response wraps the HTTP response from the Kiro API.
type Response struct {
	StatusCode   int
	Body         io.ReadCloser
	Header       http.Header
	PromptTokens int // pre-counted from serialized payload via tiktoken
}

// RefreshedCredentials is the credential snapshot a refresh produces. The
// bearer and the profile ARN are one unit: Kiro authorizes the pair, so a new
// token sent alongside the profile ARN it replaced only earns a second 403. An
// empty field means "unchanged".
type RefreshedCredentials struct {
	AccessToken string
	ProfileARN  string
}

// resolve fills unset fields from the snapshot that was just rejected.
func (r RefreshedCredentials) resolve(token, profileARN string) (string, string) {
	if r.AccessToken != "" {
		token = r.AccessToken
	}
	if r.ProfileARN != "" {
		profileARN = r.ProfileARN
	}
	return token, profileARN
}

// TokenRefresher is called when OAuth authentication receives a 403 to get a
// fresh credential snapshot. API-key authentication never invokes it because API
// keys cannot be refreshed.
type TokenRefresher func(ctx context.Context) (RefreshedCredentials, error)

// ErrBodyReadIdle is returned when the Kiro response body has not produced
// any data within the configured idle timeout. This guards against silent
// hangs where the server sends eventstream headers but never delivers frames.
var ErrBodyReadIdle = errors.New("kiroclient: body read idle timeout")

const defaultBodyReadIdleTimeout = 180 * time.Second

// HTTPClient is the production implementation of Client.
type HTTPClient struct {
	httpClient     *http.Client
	baseURL        string // override for tests; empty = use region-based URL
	mcpURL         string // override for tests; empty = use q.<region>.amazonaws.com/mcp
	region         string // pins endpoint region; empty = use the credential region
	otel           bool
	otelBodyLimit  int
	tokenRefresher TokenRefresher
	countTokens    func([]byte) (int, error) // nil = skip token counting
	bodyReadIdle   time.Duration             // idle timeout for response body reads; 0 = use default
	apiKeyAuth     bool                      // send TokenType: API_KEY with the bearer
}

// HTTPClientOption configures an HTTPClient.
type HTTPClientOption func(*HTTPClient)

// WithBaseURL sets a custom base URL (for testing).
func WithBaseURL(url string) HTTPClientOption {
	return func(c *HTTPClient) { c.baseURL = url }
}

// WithRegion pins the region used to build Kiro runtime and MCP endpoints.
// Kiro CLI credentials may contain the region where the user signed in, which
// is not necessarily a region where Kiro exposes an inference endpoint. The
// credential region remains untouched so token refresh still uses its issuer.
func WithRegion(region string) HTTPClientOption {
	return func(c *HTTPClient) { c.region = region }
}

// WithMCPURL sets a custom Kiro MCP endpoint. It is intended for tests and
// local contract verification; production callers use the region endpoint.
func WithMCPURL(url string) HTTPClientOption {
	return func(c *HTTPClient) { c.mcpURL = url }
}

// WithTokenRefresher sets the token refresh callback for 403 retry.
func WithTokenRefresher(fn TokenRefresher) HTTPClientOption {
	return func(c *HTTPClient) { c.tokenRefresher = fn }
}

// WithAPIKeyAuth marks the bearer credential as a Kiro API key rather than an
// OAuth access token, which the API distinguishes by a TokenType header. Set
// once at construction because the credential source is fixed for the process
// lifetime; this keeps it off the Client interface and out of every call site.
//
// Without the header the API treats the bearer as an OAuth token and rejects
// the call with "profileArn is required for this request" — an API key has no
// profile ARN to give.
func WithAPIKeyAuth() HTTPClientOption {
	return func(c *HTTPClient) { c.apiKeyAuth = true }
}

// WithTokenCounter sets a function to count prompt tokens from the serialized payload.
func WithTokenCounter(fn func([]byte) (int, error)) HTTPClientOption {
	return func(c *HTTPClient) { c.countTokens = fn }
}

// WithBodyReadIdleTimeout sets the idle read deadline applied to the
// response body of a successful 200 eventstream response. If no byte is
// read within the given duration, the body Read returns ErrBodyReadIdle.
//
// NOTE: The idle reader calls Close() to unblock a pending Read. This is
// guaranteed to work for net/http.Response.Body but is NOT a general
// guarantee for arbitrary io.ReadCloser implementations.
func WithBodyReadIdleTimeout(d time.Duration) HTTPClientOption {
	return func(c *HTTPClient) { c.bodyReadIdle = d }
}

// WithOTel enables OpenTelemetry tracing on outgoing requests.
func WithOTel(bodyLimit int) HTTPClientOption {
	return func(c *HTTPClient) {
		c.otel = true
		c.otelBodyLimit = bodyLimit
	}
}

// NewHTTPClient creates a new HTTPClient.
func NewHTTPClient(opts ...HTTPClientOption) *HTTPClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 10
	transport.IdleConnTimeout = 90 * time.Second
	transport.ResponseHeaderTimeout = 30 * time.Second

	c := &HTTPClient{}
	for _, opt := range opts {
		opt(c)
	}

	var rt http.RoundTripper = transport
	if c.otel {
		rt = tracing.WrapTransport(transport, c.otelBodyLimit)
	}
	c.httpClient = &http.Client{Transport: rt}
	return c
}

func (c *HTTPClient) bodyReadIdleTimeout() time.Duration {
	if c.bodyReadIdle > 0 {
		return c.bodyReadIdle
	}
	return defaultBodyReadIdleTimeout
}

// idleReader moved to idle_reader.go.

func (c *HTTPClient) recordError(ctx context.Context, err error) {
	if c.otel {
		tracing.RecordError(ctx, err)
	}
}

func (c *HTTPClient) effectiveRegion(region string) string {
	if c.region != "" {
		return c.region
	}
	return region
}

func (c *HTTPClient) endpointURL(region string) string {
	if c.baseURL != "" {
		return c.baseURL
	}
	return fmt.Sprintf("https://runtime.%s.kiro.dev/", c.effectiveRegion(region))
}

// credentialRedactor removes known bearer credentials from upstream diagnostics,
// including the escaped forms commonly used in JSON and URLs. Longest values
// must be replaced first when the original and refreshed tokens overlap.
func credentialRedactor(tokens []string) (*strings.Replacer, []string) {
	var values []string
	for _, token := range tokens {
		if token == "" {
			continue
		}
		values = append(values, token, url.QueryEscape(token), url.PathEscape(token))
		if encoded, err := json.Marshal(token); err == nil {
			values = append(values, string(encoded[1:len(encoded)-1]))
		}
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	var pairs []string
	for _, value := range values {
		pairs = append(pairs, value, "[REDACTED]")
	}
	return strings.NewReplacer(pairs...), values
}

func redactCredentials(value string, tokens []string) string {
	redactor, _ := credentialRedactor(tokens)
	return redactor.Replace(value)
}

func safeResponseHeaders(header http.Header, tokens []string) http.Header {
	result := header.Clone()
	for name, values := range result {
		for i, value := range values {
			result[name][i] = redactCredentials(value, tokens)
		}
	}
	return result
}

func readSafeErrorBody(body io.ReadCloser, tokens []string) string {
	redactor, values := credentialRedactor(tokens)
	value := readLimitedBody(body, upstreamBodyLimit+1)
	partial := 0
	if len(value) > upstreamBodyLimit {
		value = value[:upstreamBodyLimit]
		// A bounded read can split a reflected credential. Remove any matching
		// suffix before redacting complete credentials, so no prefix leaks.
		for _, credential := range values {
			for n := min(len(credential)-1, len(value)); n > partial; n-- {
				if strings.HasSuffix(value, credential[:n]) {
					partial = n
					break
				}
			}
		}
	}
	value = redactor.Replace(value[:len(value)-partial])
	if partial > 0 {
		value += "[REDACTED]"
	}
	if len(value) > upstreamBodyLimit {
		value = value[:upstreamBodyLimit]
	}
	return value
}

// GenerateAssistantResponse sends a request to the Kiro API with retry logic.
func (c *HTTPClient) GenerateAssistantResponse(ctx context.Context, token string, payload *kiroproto.Payload, region string) (*Response, error) {
	endpoint := c.endpointURL(region)

	if c.otel {
		var span trace.Span
		ctx, span = tracing.Tracer().Start(ctx, "kiro.GenerateAssistantResponse")
		defer span.End()
		span.SetAttributes(
			attribute.String("kiro.region", c.effectiveRegion(region)),
			attribute.String("kiro.endpoint", endpoint),
		)
	}

	// encode clones the payload so a refreshed profile ARN never mutates the
	// caller's copy, which the retry layer above still holds.
	encode := func(profileARN string) ([]byte, error) {
		clone := *payload
		clone.ProfileARN = profileARN
		return json.Marshal(&clone)
	}

	currentProfileARN := payload.ProfileARN
	body, err := encode(currentProfileARN)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	var promptTokens int
	if c.countTokens != nil {
		n, err := c.countTokens(body)
		if err != nil {
			slog.Debug("tokencount: failed to count prompt tokens", "err", err)
		} else {
			promptTokens = n
		}
	}

	currentToken := token
	credentialTokens := []string{token}
	authRetried := false
	invocationID := uuid.New().String()
	traceID, short := logging.TraceIDs(ctx)

	for attempt := range maxAttempts {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}

		req.Header.Set("Authorization", "Bearer "+currentToken)
		if c.apiKeyAuth {
			// Canonical spelling in Kiro's own client is "TokenType"; the header
			// name is case-insensitive on the wire.
			req.Header.Set("TokenType", "API_KEY")
		}
		req.Header.Set("Content-Type", "application/x-amz-json-1.0")
		req.Header.Set("Accept", "*/*")
		req.Header.Set("X-Amz-Target", amzTarget)
		req.Header.Set("User-Agent", userAgentValue)
		req.Header.Set("x-amz-user-agent", amzUserAgentValue)
		req.Header.Set("x-amzn-codewhisperer-optout", "false")
		req.Header.Set("amz-sdk-invocation-id", invocationID)
		req.Header.Set("amz-sdk-request", fmt.Sprintf("attempt=%d; max=%d", attempt+1, maxAttempts))

		slog.DebugContext(ctx, "kiro request headers",
			"trace_id", traceID,
			"session_id", logging.SessionIDFromContext(ctx),
			"headers", logging.SafeHeaders{H: req.Header},
		)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if attempt < maxAttempts-1 {
				delay := backoffDelay(attempt)
				slog.WarnContext(ctx, "kiro: request error, retrying",
					"trace_id", short, "attempt", attempt+1, "max", maxAttempts,
					"delay", delay, "err", err)
				if waitErr := retryWait(ctx, delay); waitErr != nil {
					return nil, waitErr
				}
				continue
			}
			c.recordError(ctx, err)
			return nil, fmt.Errorf("do request: %w", err)
		}
		resp.Header = safeResponseHeaders(resp.Header, credentialTokens)

		switch {
		case resp.StatusCode == http.StatusOK:
			slog.DebugContext(ctx, "kiro response headers",
				"trace_id", traceID,
				"session_id", logging.SessionIDFromContext(ctx),
				"status", resp.StatusCode,
				"headers", logging.SafeHeaders{H: resp.Header},
			)
			// Kiro sometimes returns 200 with Content-Type application/json
			// (AWS exception envelope such as ThrottlingException or
			// InternalServerException) instead of the expected
			// application/vnd.amazon.eventstream. Detect and surface that
			// explicitly — otherwise the eventstream parser reads a
			// non-framed body and eventually errors with a confusing
			// "reading prelude" message, masking the real upstream error.
			if ct := resp.Header.Get("Content-Type"); !isEventStreamContentType(ct) {
				errBody := readSafeErrorBody(resp.Body, credentialTokens)
				exType := resolveAWSException(errBody, resp.Header)
				// Retry transient AWS exceptions (throttling / internal / 5xx-equivalent)
				// even though the HTTP status is 200.
				if attempt < maxAttempts-1 && isRetryableAWSException(exType) {
					delay := backoffDelay(attempt)
					slog.WarnContext(ctx, "kiro: 200 with non-eventstream exception, retrying",
						"trace_id", short, "content_type", ct, "exception", exType,
						"attempt", attempt+1, "max", maxAttempts,
						"delay", delay, "body", errBody)
					if waitErr := retryWait(ctx, delay); waitErr != nil {
						return nil, waitErr
					}
					continue
				}
				ue := &UpstreamError{
					Status:      resp.StatusCode,
					ContentType: ct,
					Exception:   exType,
					Body:        errBody,
				}
				c.recordError(ctx, ue)
				return nil, ue
			}
			body := io.ReadCloser(&idleReader{rc: resp.Body, idle: c.bodyReadIdleTimeout()})
			return &Response{
				StatusCode:   resp.StatusCode,
				Body:         body,
				Header:       resp.Header,
				PromptTokens: promptTokens,
			}, nil

		case resp.StatusCode == http.StatusForbidden:
			errBody := readSafeErrorBody(resp.Body, credentialTokens)
			// One auth retry at most, and only when the refresh actually hands
			// back a different snapshot. A 403 answered with the same bearer and
			// profile ARN is Kiro's authorization decision, not an expired
			// token: resending it spends the remaining attempts on the same
			// answer and delays the real error.
			if !c.apiKeyAuth && !authRetried && attempt < maxAttempts-1 && c.tokenRefresher != nil {
				authRetried = true
				refreshed, refreshErr := c.tokenRefresher(ctx)
				nextToken, nextProfileARN := refreshed.resolve(currentToken, currentProfileARN)
				switch {
				case refreshErr != nil:
					slog.WarnContext(ctx, "kiro: token refresh failed",
						"trace_id", short, "err", redactCredentials(refreshErr.Error(), credentialTokens))
				case nextToken == currentToken && nextProfileARN == currentProfileARN:
					slog.InfoContext(ctx, "kiro: 403 received, credential snapshot unchanged",
						"trace_id", short, "attempt", attempt+1, "max", maxAttempts)
				default:
					newBody, encodeErr := encode(nextProfileARN)
					if encodeErr != nil {
						return nil, fmt.Errorf("marshal payload: %w", encodeErr)
					}
					// Bearer and profile ARN advance together or not at all.
					currentToken, currentProfileARN, body = nextToken, nextProfileARN, newBody
					credentialTokens = append(credentialTokens, nextToken)
					slog.InfoContext(ctx, "kiro: 403 received, credential snapshot refreshed",
						"trace_id", short, "attempt", attempt+1, "max", maxAttempts)
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

		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			errBody := readSafeErrorBody(resp.Body, credentialTokens)
			if attempt < maxAttempts-1 {
				delay := backoffDelay(attempt)
				slog.WarnContext(ctx, "kiro: upstream error, retrying",
					"trace_id", short, "status", resp.StatusCode,
					"attempt", attempt+1, "max", maxAttempts,
					"delay", delay, "body", errBody)
				if waitErr := retryWait(ctx, delay); waitErr != nil {
					return nil, waitErr
				}
				continue
			}
			ue := &UpstreamError{
				Status:      resp.StatusCode,
				ContentType: resp.Header.Get("Content-Type"),
				Exception:   resolveAWSException(errBody, resp.Header),
				Body:        errBody,
			}
			c.recordError(ctx, ue)
			return nil, ue

		default:
			errBody := readSafeErrorBody(resp.Body, credentialTokens)
			ue := &UpstreamError{
				Status:      resp.StatusCode,
				ContentType: resp.Header.Get("Content-Type"),
				Exception:   resolveAWSException(errBody, resp.Header),
				Body:        errBody,
			}
			c.recordError(ctx, ue)
			return nil, ue
		}
	}

	err = fmt.Errorf("kiro api: max retries exceeded")
	c.recordError(ctx, err)
	return nil, err
}
