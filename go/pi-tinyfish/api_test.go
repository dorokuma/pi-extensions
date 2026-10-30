package pitinyfish

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMissingAPIKey(t *testing.T) {
	orig := os.Getenv(EnvAPIKey)
	defer os.Setenv(EnvAPIKey, orig)
	_ = os.Unsetenv(EnvAPIKey)

	client, err := NewClient()
	if err == nil {
		t.Fatal("expected error for missing API key, got nil")
	}
	if client != nil {
		t.Fatal("expected nil client on missing API key")
	}
	expectedMsg := "Missing API key. Pass `apiKey` to the constructor or set the TINYFISH_API_KEY environment variable."
	if err.Error() != expectedMsg {
		t.Fatalf("expected error message %q, got %q", expectedMsg, err.Error())
	}
}

func TestSearchSuccess(t *testing.T) {
	var requestedURL string
	var apiKeyHeader, userAgentHeader, originHeader, acceptHeader, contentTypeHeader string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedURL = r.URL.String()
		apiKeyHeader = r.Header.Get(HeaderAPIKey)
		userAgentHeader = r.Header.Get("User-Agent")
		originHeader = r.Header.Get("X-TF-Request-Origin")
		acceptHeader = r.Header.Get("Accept")
		contentTypeHeader = r.Header.Get("Content-Type")

		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		resp := SearchResponse{
			Query:        "test query",
			TotalResults: 42,
			Page:         1,
			Results: []SearchResult{
				{
					Position: 1,
					SiteName: "Example",
					Snippet:  "An example snippet.",
					Title:    "Example Domain",
					URL:      "https://example.com",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	client, err := NewClient(
		WithBaseURL(ts.URL),
		WithAPIKey("test-dummy-key"),
	)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	page := 1
	res, err := client.Search(context.Background(), SearchParams{
		Query:    "test query",
		Location: "US",
		Language: "en",
		Page:     &page,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	if apiKeyHeader != "test-dummy-key" {
		t.Errorf("expected X-API-Key 'test-dummy-key', got %q", apiKeyHeader)
	}
	if userAgentHeader != DefaultUserAgent {
		t.Errorf("expected User-Agent %q, got %q", DefaultUserAgent, userAgentHeader)
	}
	if originHeader != HeaderRequestOrigin {
		t.Errorf("expected X-TF-Request-Origin %q, got %q", HeaderRequestOrigin, originHeader)
	}
	if acceptHeader != "application/json" {
		t.Errorf("expected Accept application/json, got %q", acceptHeader)
	}
	if contentTypeHeader != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", contentTypeHeader)
	}
	if !strings.Contains(requestedURL, "query=test+query") {
		t.Errorf("requestedURL missing query: %s", requestedURL)
	}
	if !strings.Contains(requestedURL, "location=US") {
		t.Errorf("requestedURL missing location: %s", requestedURL)
	}
	if !strings.Contains(requestedURL, "language=en") {
		t.Errorf("requestedURL missing language: %s", requestedURL)
	}
	if !strings.Contains(requestedURL, "page=1") {
		t.Errorf("requestedURL missing page: %s", requestedURL)
	}

	if res.TotalResults != 42 || res.Page != 1 || len(res.Results) != 1 {
		t.Errorf("unexpected response shape: %+v", res)
	}
	if res.Results[0].URL != "https://example.com" {
		t.Errorf("unexpected result url: %s", res.Results[0].URL)
	}
}

func TestFetchSuccess(t *testing.T) {
	var requestedBody map[string]any
	var apiKeyHeader, userAgentHeader, originHeader string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKeyHeader = r.Header.Get(HeaderAPIKey)
		userAgentHeader = r.Header.Get("User-Agent")
		originHeader = r.Header.Get("X-TF-Request-Origin")

		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		bodyBytes, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(bodyBytes, &requestedBody)

		title := "Test Page"
		desc := "A page description"
		lang := "en"
		resp := FetchResponse{
			Results: []FetchPageResult{
				{
					URL:         "https://example.com/page1",
					Title:       &title,
					Description: &desc,
					Language:    &lang,
					Format:      "markdown",
					Text:        "# Hello World\nThis is content.",
					Links:       []string{"https://example.com/about"},
				},
			},
			Errors: []FetchError{
				{
					URL:   "https://example.com/broken",
					Error: "404 Not Found",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	client, err := NewClient(
		WithBaseURL(ts.URL),
		WithAPIKey("test-dummy-key"),
	)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	linksTrue := true
	res, err := client.Fetch(context.Background(), FetchParams{
		URLs:   []string{"https://example.com/page1", "https://example.com/broken"},
		Format: "markdown",
		Links:  &linksTrue,
	})
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}

	if apiKeyHeader != "test-dummy-key" {
		t.Errorf("expected X-API-Key 'test-dummy-key', got %q", apiKeyHeader)
	}
	if userAgentHeader != DefaultUserAgent {
		t.Errorf("expected User-Agent %q, got %q", DefaultUserAgent, userAgentHeader)
	}
	if originHeader != HeaderRequestOrigin {
		t.Errorf("expected X-TF-Request-Origin %q, got %q", HeaderRequestOrigin, originHeader)
	}

	if urls, ok := requestedBody["urls"].([]any); !ok || len(urls) != 2 {
		t.Errorf("unexpected urls in request body: %v", requestedBody["urls"])
	}
	if requestedBody["format"] != "markdown" {
		t.Errorf("expected format markdown, got %v", requestedBody["format"])
	}
	if requestedBody["links"] != true {
		t.Errorf("expected links true, got %v", requestedBody["links"])
	}

	if len(res.Results) != 1 || len(res.Errors) != 1 {
		t.Fatalf("unexpected results/errors counts: %+v", res)
	}
	if res.Results[0].URL != "https://example.com/page1" {
		t.Errorf("unexpected page url: %s", res.Results[0].URL)
	}
	if res.Errors[0].Error != "404 Not Found" {
		t.Errorf("unexpected error message: %s", res.Errors[0].Error)
	}
}

func TestFetchLatencyMsIntAndFloat(t *testing.T) {
	testCases := []struct {
		name       string
		rawLatency string
		expected   float64
	}{
		{"float", "1234.5", 1234.5},
		{"int", "1200", 1200},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"results":[{"url":"https://example.com","format":"markdown","text":"x","latency_ms":%s}],"errors":[]}`, tc.rawLatency)
			}))
			defer ts.Close()

			client, err := NewClient(
				WithBaseURL(ts.URL),
				WithAPIKey("test-dummy-key"),
			)
			if err != nil {
				t.Fatalf("NewClient failed: %v", err)
			}

			res, err := client.Fetch(context.Background(), FetchParams{
				URLs: []string{"https://example.com"},
			})
			if err != nil {
				t.Fatalf("Fetch failed: %v", err)
			}
			if len(res.Results) != 1 {
				t.Fatalf("expected 1 result, got %d", len(res.Results))
			}
			lat := res.Results[0].LatencyMs
			if lat == nil {
				t.Fatal("expected non-nil LatencyMs")
			}
			f, err := lat.Float64()
			if err != nil {
				t.Fatalf("LatencyMs %q not a valid float: %v", string(*lat), err)
			}
			if f != tc.expected {
				t.Fatalf("expected LatencyMs %v, got %v", tc.expected, f)
			}
			expectedMs := int64(tc.expected + 0.5)
			if formatted := FormatFetchResponse(res); !strings.Contains(formatted, fmt.Sprintf("Latency: %dms", expectedMs)) {
				t.Fatalf("expected 'Latency: %dms' in formatted output:\n%s", expectedMs, formatted)
			}
		})
	}
}

func TestNonRetryableStatusCodes(t *testing.T) {
	testCases := []struct {
		statusCode  int
		respBody    string
		expectedMsg string
	}{
		{http.StatusBadRequest, `{"error": {"message": "Bad query syntax"}}`, "Bad query syntax"},
		{http.StatusUnauthorized, `{"message": "Invalid API key"}`, "Invalid API key"},
		{http.StatusForbidden, `Permission denied raw string`, "Permission denied raw string"},
		{http.StatusNotFound, ``, "HTTP 404"},
		{http.StatusUnprocessableEntity, `{"error": "Validation failed"}`, "Validation failed"},
	}

	for _, tc := range testCases {
		t.Run(http.StatusText(tc.statusCode), func(t *testing.T) {
			var requestCount int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&requestCount, 1)
				w.WriteHeader(tc.statusCode)
				_, _ = w.Write([]byte(tc.respBody))
			}))
			defer ts.Close()

			client, err := NewClient(
				WithBaseURL(ts.URL),
				WithAPIKey("test-dummy-key"),
			)
			if err != nil {
				t.Fatalf("NewClient failed: %v", err)
			}

			_, err = client.Search(context.Background(), SearchParams{Query: "test"})
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if atomic.LoadInt32(&requestCount) != 1 {
				t.Fatalf("expected exactly 1 request (no retry) for %d, got %d", tc.statusCode, requestCount)
			}
			if !strings.Contains(err.Error(), tc.expectedMsg) {
				t.Fatalf("expected error to contain %q, got %q", tc.expectedMsg, err.Error())
			}
		})
	}
}

func TestRetryableStatusCodesUntilExhaustion(t *testing.T) {
	retryableStatuses := []int{
		http.StatusRequestTimeout,      // 408
		http.StatusTooManyRequests,     // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
	}

	for _, statusCode := range retryableStatuses {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			var requestCount int32
			var sleeps []time.Duration

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&requestCount, 1)
				w.WriteHeader(statusCode)
				_, _ = w.Write([]byte(`{"error": {"message": "Transient failure"}}`))
			}))
			defer ts.Close()

			client, err := NewClient(
				WithBaseURL(ts.URL),
				WithAPIKey("test-dummy-key"),
				WithSleeper(func(ctx context.Context, d time.Duration) error {
					sleeps = append(sleeps, d)
					return nil
				}),
			)
			if err != nil {
				t.Fatalf("NewClient failed: %v", err)
			}

			_, err = client.Search(context.Background(), SearchParams{Query: "test"})
			if err == nil {
				t.Fatal("expected error after retries exhausted, got nil")
			}

			// Initial attempt + 2 retries = 3 requests
			if atomic.LoadInt32(&requestCount) != 3 {
				t.Fatalf("expected 3 requests (1 initial + 2 retries), got %d", requestCount)
			}

			if len(sleeps) != 2 {
				t.Fatalf("expected 2 sleeps, got %d", len(sleeps))
			}
			if sleeps[0] != 500*time.Millisecond {
				t.Errorf("expected first sleep 500ms, got %v", sleeps[0])
			}
			if sleeps[1] != 1000*time.Millisecond {
				t.Errorf("expected second sleep 1000ms, got %v", sleeps[1])
			}
			if !strings.Contains(err.Error(), "Transient failure") {
				t.Errorf("expected error message Transient failure, got %v", err)
			}
		})
	}
}

func TestTimeoutSpanningRetries(t *testing.T) {
	var reqCount atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		// Server takes 50ms
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"query":"test","results":[],"total_results":0,"page":0}`))
	}))
	defer ts.Close()

	// Timeout set to 10ms (shorter than server response time)
	client, err := NewClient(
		WithBaseURL(ts.URL),
		WithAPIKey("test-dummy-key"),
		WithTimeout(10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	_, err = client.Search(context.Background(), SearchParams{Query: "test"})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "Request timed out after 10ms") {
		t.Fatalf("expected 'Request timed out after 10ms', got %q", err.Error())
	}
	if c := reqCount.Load(); c != 1 {
		t.Fatalf("expected exactly 1 request before timeout, got %d", c)
	}
}

func TestAPIIntegrationInjection(t *testing.T) {
	var capturedBody map[string]any

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(bodyBytes, &capturedBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[],"errors":[]}`))
	}))
	defer ts.Close()

	// Case 1: TF_API_INTEGRATION is present
	_ = os.Setenv(EnvAPIIntegration, "custom_harness_v1")
	defer os.Unsetenv(EnvAPIIntegration)

	client, _ := NewClient(WithBaseURL(ts.URL), WithAPIKey("dummy"))
	_, err := client.Fetch(context.Background(), FetchParams{
		URLs: []string{"https://example.com"},
	})
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	if capturedBody["api_integration"] != "custom_harness_v1" {
		t.Fatalf("expected api_integration 'custom_harness_v1', got %v", capturedBody["api_integration"])
	}

	// Case 2: TF_API_INTEGRATION is absent
	_ = os.Unsetenv(EnvAPIIntegration)
	capturedBody = nil
	_, err = client.Fetch(context.Background(), FetchParams{
		URLs: []string{"https://example.com"},
	})
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	if _, ok := capturedBody["api_integration"]; ok {
		t.Fatalf("expected api_integration to be absent when env is unset, got %v", capturedBody["api_integration"])
	}
}
