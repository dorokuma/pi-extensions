// Package pitinyfish implements the TinyFish search and fetch extension for PiG in pure Go.
// It directly interfaces with the TinyFish HTTP API using net/http without third-party dependencies.
package pitinyfish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the upstream TinyFish API endpoint.
	DefaultBaseURL = "https://agent.tinyfish.ai"

	// DefaultUserAgent matches the TypeScript SDK User-Agent string.
	DefaultUserAgent = "tinyfish-typescript/0.0.8"

	// HeaderRequestOrigin matches the TypeScript SDK custom header.
	HeaderRequestOrigin = "tinyfish-js"

	// HeaderAPIKey is the HTTP header used to carry the API key.
	HeaderAPIKey = "X-API-Key"

	// EnvAPIKey is the environment variable for the TinyFish API key.
	EnvAPIKey = "TINYFISH_API_KEY"

	// EnvAPIIntegration is the environment variable for injecting api_integration metadata into request bodies.
	EnvAPIIntegration = "TF_API_INTEGRATION"

	// DefaultSearchTimeout is the client timeout used for search operations (10s in pi-tinyfish).
	DefaultSearchTimeout = 10 * time.Second

	// DefaultFetchTimeout is the client timeout used for fetch operations (150s in pi-tinyfish).
	DefaultFetchTimeout = 150 * time.Second

	// DefaultMaxRetries is the maximum number of retry attempts (matches SDK DEFAULT_MAX_RETRIES = 2).
	DefaultMaxRetries = 2

	// DefaultMinRetryTimeout is the initial backoff delay (matches p-retry minTimeout = 500ms).
	DefaultMinRetryTimeout = 500 * time.Millisecond

	// DefaultMaxRetryTimeout is the maximum backoff delay (matches p-retry maxTimeout = 8000ms).
	DefaultMaxRetryTimeout = 8000 * time.Millisecond

	// DefaultRetryFactor is the exponential backoff multiplier (matches p-retry factor = 2).
	DefaultRetryFactor = 2.0
)

// SearchParams contains the input options for a search query.
type SearchParams struct {
	Query    string
	Location string
	Language string
	Page     *int
}

// SearchResult represents a single search result returned by the API.
type SearchResult struct {
	Position int    `json:"position"`
	SiteName string `json:"site_name"`
	Snippet  string `json:"snippet"`
	Title    string `json:"title"`
	URL      string `json:"url"`
}

// SearchResponse represents the public response payload from GET /v1/search.
type SearchResponse struct {
	Query        string         `json:"query"`
	Results      []SearchResult `json:"results"`
	TotalResults int            `json:"total_results"`
	Page         int            `json:"page"`
}

// FetchParams contains the input options for fetching page contents.
type FetchParams struct {
	URLs       []string
	Format     string // "markdown", "html", or "json"
	Links      *bool
	ImageLinks *bool
}

// FetchPageResult represents a single successfully fetched and extracted page.
type FetchPageResult struct {
	URL           string       `json:"url"`
	FinalURL      *string      `json:"final_url,omitempty"`
	Title         *string      `json:"title,omitempty"`
	Description   *string      `json:"description,omitempty"`
	Language      *string      `json:"language,omitempty"`
	Author        *string      `json:"author,omitempty"`
	PublishedDate *string      `json:"published_date,omitempty"`
	Format        string       `json:"format"`
	Text          any          `json:"text,omitempty"`
	Links         []string     `json:"links,omitempty"`
	ImageLinks    []string     `json:"image_links,omitempty"`
	LatencyMs     *json.Number `json:"latency_ms,omitempty"`
}

// Milliseconds returns the latency in milliseconds as an int64, rounding to
// the nearest millisecond. It returns 0 when the latency is absent or cannot
// be parsed as a number.
func (p *FetchPageResult) Milliseconds() int64 {
	if p == nil || p.LatencyMs == nil {
		return 0
	}
	if i, err := p.LatencyMs.Int64(); err == nil {
		return i
	}
	f, err := p.LatencyMs.Float64()
	if err != nil {
		return 0
	}
	return int64(f + 0.5)
}

// FetchError represents an error encountered while fetching an individual URL.
type FetchError struct {
	URL   string `json:"url"`
	Error string `json:"error"`
}

// FetchResponse represents the public response payload from POST /v1/fetch.
type FetchResponse struct {
	Results []FetchPageResult `json:"results"`
	Errors  []FetchError      `json:"errors"`
}

// SleeperFunc is a custom sleep function supporting cancellation.
type SleeperFunc func(ctx context.Context, d time.Duration) error

func defaultSleeper(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Client is the direct HTTP client for the TinyFish API.
type Client struct {
	baseURL         string
	apiKey          string
	userAgent       string
	timeout         time.Duration
	maxRetries      int
	minRetryTimeout time.Duration
	maxRetryTimeout time.Duration
	retryFactor     float64
	httpClient      *http.Client
	sleeper         SleeperFunc
}

// ClientOption configures a Client instance.
type ClientOption func(*Client)

// WithBaseURL overrides the default API base URL.
func WithBaseURL(u string) ClientOption {
	return func(c *Client) {
		if u != "" {
			c.baseURL = strings.TrimRight(u, "/")
		}
	}
}

// WithAPIKey sets an explicit API key.
func WithAPIKey(key string) ClientOption {
	return func(c *Client) {
		c.apiKey = strings.TrimSpace(key)
	}
}

// WithTimeout sets the request timeout spanning the entire retry sequence.
func WithTimeout(d time.Duration) ClientOption {
	return func(c *Client) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// WithMaxRetries sets the maximum number of retry attempts.
func WithMaxRetries(n int) ClientOption {
	return func(c *Client) {
		if n >= 0 {
			c.maxRetries = n
		}
	}
}

// WithHTTPClient overrides the underlying http.Client.
func WithHTTPClient(client *http.Client) ClientOption {
	return func(c *Client) {
		if client != nil {
			c.httpClient = client
		}
	}
}

// WithSleeper sets a custom sleeper function, useful for tests.
func WithSleeper(sleeper SleeperFunc) ClientOption {
	return func(c *Client) {
		if sleeper != nil {
			c.sleeper = sleeper
		}
	}
}

// NewClient creates a new TinyFish HTTP client.
// If no API key is passed via options, it reads from the TINYFISH_API_KEY environment variable.
// Returns an error if the API key is missing. The API key is never printed or leaked.
func NewClient(opts ...ClientOption) (*Client, error) {
	c := &Client{
		baseURL:         DefaultBaseURL,
		userAgent:       DefaultUserAgent,
		timeout:         DefaultSearchTimeout,
		maxRetries:      DefaultMaxRetries,
		minRetryTimeout: DefaultMinRetryTimeout,
		maxRetryTimeout: DefaultMaxRetryTimeout,
		retryFactor:     DefaultRetryFactor,
		httpClient:      &http.Client{},
		sleeper:         defaultSleeper,
	}

	for _, opt := range opts {
		opt(c)
	}

	if c.apiKey == "" {
		c.apiKey = strings.TrimSpace(os.Getenv(EnvAPIKey))
	}

	if c.apiKey == "" {
		return nil, errors.New("Missing API key. Pass `apiKey` to the constructor or set the TINYFISH_API_KEY environment variable.")
	}

	return c, nil
}

// Search executes a GET /v1/search request.
func (c *Client) Search(ctx context.Context, params SearchParams) (*SearchResponse, error) {
	query := strings.TrimSpace(params.Query)
	if query == "" {
		return nil, errors.New("No query provided.")
	}
	if params.Page != nil {
		if *params.Page < 0 {
			return nil, errors.New("page must be >= 0")
		}
		if *params.Page > 10 {
			return nil, errors.New("page must be <= 10")
		}
	}

	q := url.Values{}
	q.Set("query", query)
	if loc := strings.TrimSpace(params.Location); loc != "" {
		q.Set("location", loc)
	}
	if lang := strings.TrimSpace(params.Language); lang != "" {
		q.Set("language", lang)
	}
	if params.Page != nil {
		q.Set("page", strconv.Itoa(*params.Page))
	}

	path := "/v1/search?" + q.Encode()
	respBody, err := c.doRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}

	var res SearchResponse
	if err := json.Unmarshal(respBody, &res); err != nil {
		return nil, fmt.Errorf("failed to parse search response: %w", err)
	}
	return &res, nil
}

// Fetch executes a POST /v1/fetch request.
func (c *Client) Fetch(ctx context.Context, params FetchParams) (*FetchResponse, error) {
	if len(params.URLs) == 0 {
		return nil, errors.New("urls must be a non-empty array")
	}
	if len(params.URLs) > 10 {
		return nil, errors.New("urls must contain at most 10 items")
	}
	if params.Format != "" && params.Format != "markdown" && params.Format != "html" && params.Format != "json" {
		return nil, fmt.Errorf("invalid format: %s", params.Format)
	}

	reqMap := map[string]any{
		"urls": params.URLs,
	}
	if params.Format != "" {
		reqMap["format"] = params.Format
	}
	if params.Links != nil {
		reqMap["links"] = *params.Links
	}
	if params.ImageLinks != nil {
		reqMap["image_links"] = *params.ImageLinks
	}

	// Inject api_integration from TF_API_INTEGRATION environment variable if present.
	if integration := strings.TrimSpace(os.Getenv(EnvAPIIntegration)); integration != "" {
		reqMap["api_integration"] = integration
	}

	bodyBytes, err := json.Marshal(reqMap)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize fetch request: %w", err)
	}

	respBody, err := c.doRequest(ctx, http.MethodPost, "/v1/fetch", bodyBytes)
	if err != nil {
		return nil, err
	}

	var res FetchResponse
	if err := json.Unmarshal(respBody, &res); err != nil {
		return nil, fmt.Errorf("failed to parse fetch response: %w", err)
	}
	return &res, nil
}

// doRequest handles headers, total timeout, retries, and error mapping.
func (c *Client) doRequest(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	// The client timeout spans the entire retry sequence, matching SDK behavior.
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	backoff := c.minRetryTimeout

	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		// Check context before initiating attempt
		if reqCtx.Err() != nil {
			if errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("Request timed out after %dms", c.timeout.Milliseconds())
			}
			return nil, errors.New("Request aborted")
		}

		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(body)
		}

		req, err := http.NewRequestWithContext(reqCtx, method, c.baseURL+path, bodyReader)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}

		req.Header.Set(HeaderAPIKey, c.apiKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", c.userAgent)
		req.Header.Set("X-TF-Request-Origin", HeaderRequestOrigin)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("Request timed out after %dms", c.timeout.Milliseconds())
			}
			if errors.Is(err, context.Canceled) || errors.Is(reqCtx.Err(), context.Canceled) {
				return nil, errors.New("Request aborted")
			}
			// Network error (DNS, connection refused, reset)
			if attempt == c.maxRetries {
				return nil, err
			}
			if sleepErr := c.sleeper(reqCtx, backoff); sleepErr != nil {
				if errors.Is(sleepErr, context.DeadlineExceeded) || errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
					return nil, fmt.Errorf("Request timed out after %dms", c.timeout.Milliseconds())
				}
				return nil, errors.New("Request aborted")
			}
			backoff = time.Duration(float64(backoff) * c.retryFactor)
			if backoff > c.maxRetryTimeout {
				backoff = c.maxRetryTimeout
			}
			continue
		}

		respBody, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			if attempt == c.maxRetries {
				return nil, fmt.Errorf("failed to read response body: %w", readErr)
			}
			if sleepErr := c.sleeper(reqCtx, backoff); sleepErr != nil {
				return nil, fmt.Errorf("Request timed out after %dms", c.timeout.Milliseconds())
			}
			backoff = time.Duration(float64(backoff) * c.retryFactor)
			if backoff > c.maxRetryTimeout {
				backoff = c.maxRetryTimeout
			}
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return respBody, nil
		}

		// Check if error is retryable (408, 429, 5xx)
		statusCode := resp.StatusCode
		retryable := (statusCode == 408 || statusCode == 429 || statusCode >= 500)
		if !retryable || attempt == c.maxRetries {
			errMsg := parseErrorMessage(statusCode, respBody)
			return nil, errors.New(errMsg)
		}

		// Wait before retrying
		if sleepErr := c.sleeper(reqCtx, backoff); sleepErr != nil {
			if errors.Is(sleepErr, context.DeadlineExceeded) || errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("Request timed out after %dms", c.timeout.Milliseconds())
			}
			return nil, errors.New("Request aborted")
		}

		backoff = time.Duration(float64(backoff) * c.retryFactor)
		if backoff > c.maxRetryTimeout {
			backoff = c.maxRetryTimeout
		}
	}

	return nil, errors.New("maximum retries exceeded")
}

// parseErrorMessage extracts a human-readable error from the response body,
// mirroring the Python/TypeScript SDK _parse_error_message behavior.
func parseErrorMessage(statusCode int, body []byte) string {
	if len(body) > 0 {
		var bodyObj map[string]any
		if err := json.Unmarshal(body, &bodyObj); err == nil {
			if errVal, ok := bodyObj["error"]; ok {
				if errMap, ok := errVal.(map[string]any); ok {
					if msg, ok := errMap["message"].(string); ok && msg != "" {
						return msg
					}
				} else if msg, ok := errVal.(string); ok && msg != "" {
					return msg
				}
			}
			if msg, ok := bodyObj["message"].(string); ok && msg != "" {
				return msg
			}
		}
		rawStr := strings.TrimSpace(string(body))
		if rawStr != "" {
			return rawStr
		}
	}
	return fmt.Sprintf("HTTP %d", statusCode)
}
