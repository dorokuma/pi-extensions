package pitinyfish

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

func TestToolRegistration(t *testing.T) {
	ext := Extension()
	if ext == nil {
		t.Fatal("Extension() returned nil")
	}

	searchDef := SearchToolDefinition()
	if searchDef.Name != "tinyfish_search" {
		t.Errorf("unexpected name: %s", searchDef.Name)
	}
	if searchDef.Label != "TinyFish Search" {
		t.Errorf("unexpected label: %s", searchDef.Label)
	}
	if !strings.Contains(searchDef.Description, "Requires TINYFISH_API_KEY") {
		t.Errorf("expected Description to mention TINYFISH_API_KEY: %s", searchDef.Description)
	}
	if len(searchDef.PromptGuidelines) != 3 {
		t.Errorf("expected 3 prompt guidelines, got %d", len(searchDef.PromptGuidelines))
	}
	if searchDef.Execute == nil {
		t.Fatal("searchDef.Execute is nil")
	}

	fetchDef := FetchToolDefinition()
	if fetchDef.Name != "tinyfish_fetch" {
		t.Errorf("unexpected name: %s", fetchDef.Name)
	}
	if fetchDef.Label != "TinyFish Fetch" {
		t.Errorf("unexpected label: %s", fetchDef.Label)
	}
	if !strings.Contains(fetchDef.Description, "Requires TINYFISH_API_KEY") {
		t.Errorf("expected Description to mention TINYFISH_API_KEY: %s", fetchDef.Description)
	}
	if len(fetchDef.PromptGuidelines) != 3 {
		t.Errorf("expected 3 prompt guidelines, got %d", len(fetchDef.PromptGuidelines))
	}
	if fetchDef.Execute == nil {
		t.Fatal("fetchDef.Execute is nil")
	}
}

func TestNormalizeUrls(t *testing.T) {
	// Case 1: url single
	urls := NormalizeUrls("  https://a.com  ", nil)
	if len(urls) != 1 || urls[0] != "https://a.com" {
		t.Fatalf("unexpected urls: %v", urls)
	}

	// Case 2: url + urls with duplicates and empty strings
	rawURLs := []any{
		"https://a.com",
		"  https://b.com  ",
		"",
		"   ",
		"https://a.com",
		"https://c.com",
	}
	urls = NormalizeUrls("https://a.com", rawURLs)
	if len(urls) != 3 {
		t.Fatalf("expected 3 urls, got %d (%v)", len(urls), urls)
	}
	if urls[0] != "https://a.com" || urls[1] != "https://b.com" || urls[2] != "https://c.com" {
		t.Fatalf("unexpected order or values: %v", urls)
	}
}

func TestNormalizeNumber(t *testing.T) {
	if v := NormalizeNumber(nil, 50, 10, 100); v != 50 {
		t.Errorf("expected fallback 50, got %d", v)
	}
	if v := NormalizeNumber(5, 50, 10, 100); v != 10 {
		t.Errorf("expected clamped min 10, got %d", v)
	}
	if v := NormalizeNumber(200, 50, 10, 100); v != 100 {
		t.Errorf("expected clamped max 100, got %d", v)
	}
	if v := NormalizeNumber(42.8, 50, 10, 100); v != 42 {
		t.Errorf("expected floored 42, got %d", v)
	}
	if v := NormalizeNumber("invalid", 50, 10, 100); v != 50 {
		t.Errorf("expected fallback 50, got %d", v)
	}
}

func TestSearchParameterValidation(t *testing.T) {
	// 1. Empty query
	resRaw, _ := ExecuteSearch(sdk.Context{}, map[string]any{"query": "   "})
	res := resRaw.(sdk.ToolResult)
	if !strings.HasPrefix(res.Content, "Error: No query provided.") {
		t.Errorf("expected 'Error: No query provided.', got %s", res.Content)
	}
}

func TestFetchParameterValidation(t *testing.T) {
	// 1. URLs empty
	resRaw, _ := ExecuteFetch(sdk.Context{}, map[string]any{})
	res := resRaw.(sdk.ToolResult)
	if !strings.HasPrefix(res.Content, "Error: No URL provided.") {
		t.Errorf("expected 'Error: No URL provided.', got %s", res.Content)
	}

	// 2. URLs > 10 (11 items)
	var elevenURLs []any
	for i := 1; i <= 11; i++ {
		elevenURLs = append(elevenURLs, fmt.Sprintf("https://example.com/%d", i))
	}
	resRaw, _ = ExecuteFetch(sdk.Context{}, map[string]any{"urls": elevenURLs})
	res = resRaw.(sdk.ToolResult)
	if !strings.Contains(res.Content, "accepts at most 10 URLs") {
		t.Errorf("expected accepts at most 10 URLs, got %s", res.Content)
	}
}

func TestFormatSearchResponse(t *testing.T) {
	// Case 1: Empty results
	emptyResp := &SearchResponse{
		Query:        "no results query",
		TotalResults: 0,
		Page:         0,
		Results:      nil,
	}
	out := FormatSearchResponse(emptyResp, SearchFormatOptions{})
	expectedLines := []string{
		"# TinyFish Search",
		"Query: no results query",
		"Page: 0",
		"Total results: 0",
		"",
		"No results returned.",
	}
	if out != strings.Join(expectedLines, "\n") {
		t.Errorf("unexpected empty output:\n%s", out)
	}

	// Case 2: Populated results with location and language
	popResp := &SearchResponse{
		Query:        "golang pi",
		TotalResults: 100,
		Page:         1,
		Results: []SearchResult{
			{
				Position: 1,
				Title:    "Go Programming",
				URL:      "https://go.dev",
				SiteName: "go.dev",
				Snippet:  "Build simple, secure, scalable systems.",
			},
			{
				Position: 2,
				Title:    "Pi Coding Agent",
				URL:      "https://example.com/pi",
				SiteName: "example.com",
				Snippet:  "",
			},
		},
	}
	outPop := FormatSearchResponse(popResp, SearchFormatOptions{
		Location: "US",
		Language: "en",
	})
	if !strings.Contains(outPop, "Location: US") || !strings.Contains(outPop, "Language: en") {
		t.Errorf("missing location/language in output:\n%s", outPop)
	}
	if !strings.Contains(outPop, "## 1. Go Programming\nURL: https://go.dev\nSite: go.dev\nSnippet: Build simple, secure, scalable systems.") {
		t.Errorf("missing result 1 in output:\n%s", outPop)
	}
	if !strings.Contains(outPop, "## 2. Pi Coding Agent\nURL: https://example.com/pi\nSite: example.com") {
		t.Errorf("missing result 2 in output:\n%s", outPop)
	}
	// Result 2 has empty snippet, verify "Snippet:" is omitted
	if strings.Contains(outPop, "Snippet: \n") || strings.HasSuffix(outPop, "Snippet:") {
		t.Errorf("snippet line should be omitted when empty in:\n%s", outPop)
	}
}

func TestFormatFetchResponse(t *testing.T) {
	title1 := "Page Markdown"
	finalURL1 := "https://example.com/dest"
	desc1 := "Description"
	lang1 := "zh"
	author1 := "Alice"
	pubDate1 := "2026-10-23"
	latency1 := int64(120)

	// Create 55 links to test link preview truncation at 50
	var manyLinks []string
	for i := 1; i <= 55; i++ {
		manyLinks = append(manyLinks, fmt.Sprintf("https://link.com/%d", i))
	}

	title2 := "Page HTML"
	title3 := "Page JSON"
	jsonText := map[string]any{"status": "ok", "count": 1}

	resp := &FetchResponse{
		Results: []FetchPageResult{
			{
				URL:           "https://example.com/page1",
				FinalURL:      &finalURL1,
				Title:         &title1,
				Description:   &desc1,
				Language:      &lang1,
				Author:        &author1,
				PublishedDate: &pubDate1,
				LatencyMs:     &latency1,
				Format:        "markdown",
				Text:          "Content in markdown",
				Links:         manyLinks,
			},
			{
				URL:    "https://example.com/page2",
				Title:  &title2,
				Format: "html",
				Text:   "<div>hello</div>",
			},
			{
				URL:    "https://example.com/page3",
				Title:  &title3,
				Format: "json",
				Text:   jsonText,
			},
		},
		Errors: []FetchError{
			{
				URL:   "https://example.com/err",
				Error: "500 Internal Server Error",
			},
		},
	}

	formatted := FormatFetchResponse(resp)

	if !strings.Contains(formatted, "# TinyFish Fetch\nSuccessful: 3\nErrors: 1") {
		t.Errorf("header missing in:\n%s", formatted)
	}
	if !strings.Contains(formatted, "Final URL: https://example.com/dest") {
		t.Errorf("missing final url in:\n%s", formatted)
	}
	if !strings.Contains(formatted, "Latency: 120ms") {
		t.Errorf("missing latency in:\n%s", formatted)
	}
	if !strings.Contains(formatted, "Links: 55") || !strings.Contains(formatted, "- ... 5 more") {
		t.Errorf("missing links preview truncation in:\n%s", formatted)
	}
	if !strings.Contains(formatted, "```html\n<div>hello</div>\n```") {
		t.Errorf("missing html code block in:\n%s", formatted)
	}
	if !strings.Contains(formatted, "```json\n{\n  \"count\": 1,\n  \"status\": \"ok\"\n}\n```") {
		t.Errorf("missing json code block in:\n%s", formatted)
	}
	if !strings.Contains(formatted, "## Errors\n- https://example.com/err: 500 Internal Server Error") {
		t.Errorf("missing errors block in:\n%s", formatted)
	}
}

func TestTruncationAndFormatting(t *testing.T) {
	shortText := "Line 1\nLine 2\nLine 3"
	resShort := TruncateToolText(shortText, 50000)
	if resShort.Details.Truncated {
		t.Errorf("expected not truncated")
	}
	if resShort.Text != shortText {
		t.Errorf("expected %q, got %q", shortText, resShort.Text)
	}

	longText := strings.Repeat("A very long line of characters that exceeds small byte limits.\n", 100)
	resLong := TruncateToolText(longText, 200)
	if !resLong.Details.Truncated {
		t.Fatalf("expected truncated=true")
	}
	if !strings.Contains(resLong.Text, "[Output truncated:") {
		t.Errorf("expected notice in truncated text:\n%s", resLong.Text)
	}
	if !strings.Contains(resLong.Text, "Use maxBytes to raise the limit, or narrow the request.]") {
		t.Errorf("missing notice footer in:\n%s", resLong.Text)
	}
}

func TestExecuteSearchWithFakeServer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := SearchResponse{
			Query:        "fake search",
			TotalResults: 1,
			Page:         0,
			Results: []SearchResult{
				{
					Position: 1,
					Title:    "Result 1",
					URL:      "https://res1.com",
					SiteName: "Res1",
					Snippet:  "Snippet 1",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	restore := setClientFactory(func(opts ...ClientOption) (*Client, error) {
		var allOpts []ClientOption
		allOpts = append(allOpts, WithBaseURL(ts.URL), WithAPIKey("dummy-key"))
		allOpts = append(allOpts, opts...)
		return NewClient(allOpts...)
	})
	defer restore()

	resRaw, err := ExecuteSearch(sdk.Context{}, map[string]any{"query": "fake search"})
	if err != nil {
		t.Fatalf("Execute returned unexpected err: %v", err)
	}

	res := resRaw.(sdk.ToolResult)
	if !strings.Contains(res.Content, "Result 1") {
		t.Fatalf("expected content to contain 'Result 1', got:\n%s", res.Content)
	}

	details := res.Details.(map[string]any)
	if details["provider"] != "tinyfish" || details["kind"] != "search" {
		t.Errorf("unexpected details: %+v", details)
	}
	if details["returnedResults"] != 1 || details["totalResults"] != 1 {
		t.Errorf("unexpected results count: %+v", details)
	}
}

func TestExecuteFetchWithFakeServer(t *testing.T) {
	title := "Fetched Page"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := FetchResponse{
			Results: []FetchPageResult{
				{
					URL:    "https://example.com/test",
					Title:  &title,
					Format: "markdown",
					Text:   "Extracted text content.",
				},
			},
			Errors: nil,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	restore := setClientFactory(func(opts ...ClientOption) (*Client, error) {
		var allOpts []ClientOption
		allOpts = append(allOpts, WithBaseURL(ts.URL), WithAPIKey("dummy-key"))
		allOpts = append(allOpts, opts...)
		return NewClient(allOpts...)
	})
	defer restore()

	resRaw, err := ExecuteFetch(sdk.Context{}, map[string]any{"url": "https://example.com/test"})
	if err != nil {
		t.Fatalf("Execute returned unexpected err: %v", err)
	}

	res := resRaw.(sdk.ToolResult)
	if !strings.Contains(res.Content, "Fetched Page") {
		t.Fatalf("expected content to contain 'Fetched Page', got:\n%s", res.Content)
	}

	details := res.Details.(map[string]any)
	if details["provider"] != "tinyfish" || details["kind"] != "fetch" {
		t.Errorf("unexpected details: %+v", details)
	}
	if details["successful"] != 1 || details["urlCount"] != 1 {
		t.Errorf("unexpected counts: %+v", details)
	}
}

func TestRenderers(t *testing.T) {
	ctx := sdk.Context{}

	// 1. RenderSearchCall
	lines, err := RenderSearchCall(ctx, map[string]any{"query": "pi go port"}, sdk.ToolRenderContext{}, 80)
	if err != nil || len(lines) != 1 || lines[0] != "tinyfish_search pi go port" {
		t.Fatalf("unexpected search call line: %v, %v", lines, err)
	}

	// 2. RenderFetchCall
	lines, err = RenderFetchCall(ctx, map[string]any{"url": "https://example.com"}, sdk.ToolRenderContext{}, 80)
	if err != nil || len(lines) != 1 || lines[0] != "tinyfish_fetch https://example.com" {
		t.Fatalf("unexpected fetch call line: %v, %v", lines, err)
	}

	// 3. RenderSearchResult - Collapsed
	searchResult := sdk.ToolRenderResult{
		Content: []map[string]any{{"type": "text", "text": "result content"}},
		Details: map[string]any{
			"kind":            "search",
			"returnedResults": 5,
			"totalResults":    10,
			"query":           "test query",
		},
	}
	lines, err = RenderSearchResult(ctx, searchResult, sdk.ToolRenderResultOptions{Expanded: false}, sdk.ToolRenderContext{}, 80)
	if err != nil || len(lines) != 1 || lines[0] != "5/10 results for test query" {
		t.Fatalf("unexpected collapsed search render: %v, %v", lines, err)
	}

	// 4. RenderFetchResult - Collapsed
	fetchResult := sdk.ToolRenderResult{
		Content: []map[string]any{{"type": "text", "text": "result content"}},
		Details: map[string]any{
			"kind":       "fetch",
			"successful": 2,
			"urlCount":   3,
			"errors":     []any{"err1"},
		},
	}
	lines, err = RenderFetchResult(ctx, fetchResult, sdk.ToolRenderResultOptions{Expanded: false}, sdk.ToolRenderContext{}, 80)
	if err != nil || len(lines) != 1 || lines[0] != "2/3 fetched, 1 errors" {
		t.Fatalf("unexpected collapsed fetch render: %v, %v", lines, err)
	}

	// 5. RenderResult - Partial
	lines, err = RenderSearchResult(ctx, searchResult, sdk.ToolRenderResultOptions{IsPartial: true}, sdk.ToolRenderContext{}, 80)
	if err != nil || len(lines) != 1 || lines[0] != "result content" {
		t.Fatalf("unexpected partial render: %v, %v", lines, err)
	}

	// 6. RenderResult - Error
	errResult := sdk.ToolRenderResult{
		Content: []map[string]any{{"type": "text", "text": "Error: request failed\nline 2"}},
		Details: map[string]any{"error": "request failed"},
	}
	lines, err = RenderSearchResult(ctx, errResult, sdk.ToolRenderResultOptions{}, sdk.ToolRenderContext{}, 80)
	if err != nil || len(lines) != 1 || lines[0] != "Error: request failed" {
		t.Fatalf("unexpected error render: %v, %v", lines, err)
	}

	// 7. RenderResult - Expanded
	lines, err = RenderSearchResult(ctx, searchResult, sdk.ToolRenderResultOptions{Expanded: true}, sdk.ToolRenderContext{}, 80)
	if err != nil || len(lines) != 1 || lines[0] != "result content" {
		t.Fatalf("unexpected expanded render: %v, %v", lines, err)
	}
}

func TestExecuteSearchPageClampingWithFakeServer(t *testing.T) {
	var capturedPages []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPages = append(capturedPages, r.URL.Query().Get("page"))
		resp := SearchResponse{
			Query:        "test",
			TotalResults: 0,
			Page:         0,
			Results:      nil,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	restore := setClientFactory(func(opts ...ClientOption) (*Client, error) {
		var allOpts []ClientOption
		allOpts = append(allOpts, WithBaseURL(ts.URL), WithAPIKey("dummy-key"))
		allOpts = append(allOpts, opts...)
		return NewClient(allOpts...)
	})
	defer restore()

	// page = -1 should clamp to 0
	_, err := ExecuteSearch(sdk.Context{}, map[string]any{"query": "test", "page": -1})
	if err != nil {
		t.Fatalf("unexpected error for page -1: %v", err)
	}

	// page = 15 should clamp to 10
	_, err = ExecuteSearch(sdk.Context{}, map[string]any{"query": "test", "page": 15})
	if err != nil {
		t.Fatalf("unexpected error for page 15: %v", err)
	}

	if len(capturedPages) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(capturedPages))
	}
	if capturedPages[0] != "0" {
		t.Errorf("expected clamped page '0', got %q", capturedPages[0])
	}
	if capturedPages[1] != "10" {
		t.Errorf("expected clamped page '10', got %q", capturedPages[1])
	}
}

func TestExecuteFetchInvalidFormatFallback(t *testing.T) {
	var capturedBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(bodyBytes, &capturedBody)
		title := "Example"
		resp := FetchResponse{
			Results: []FetchPageResult{
				{
					URL:    "https://example.com",
					Title:  &title,
					Format: "markdown",
					Text:   "Fallback markdown content.",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	restore := setClientFactory(func(opts ...ClientOption) (*Client, error) {
		var allOpts []ClientOption
		allOpts = append(allOpts, WithBaseURL(ts.URL), WithAPIKey("dummy-key"))
		allOpts = append(allOpts, opts...)
		return NewClient(allOpts...)
	})
	defer restore()

	// Calling ExecuteFetch with invalid format="xml" should succeed and silently fall back to markdown
	resRaw, err := ExecuteFetch(sdk.Context{}, map[string]any{
		"url":    "https://example.com",
		"format": "xml",
	})
	if err != nil {
		t.Fatalf("unexpected error for invalid format: %v", err)
	}
	res := resRaw.(sdk.ToolResult)
	if strings.Contains(res.Content, "Error:") {
		t.Fatalf("expected no error for invalid format fallback, got: %s", res.Content)
	}
	// Verify that format was omitted from request body (or fallback to default)
	if formatVal, ok := capturedBody["format"]; ok && formatVal != nil && formatVal != "" {
		t.Errorf("expected format to be omitted on invalid input, got %v", formatVal)
	}
}

func TestRenderCallChineseUTF8Truncation(t *testing.T) {
	// A long Chinese query > 80 runes (105 Chinese characters)
	longChinese := strings.Repeat("中文字符测试标", 15)
	lines, err := RenderSearchCall(sdk.Context{}, map[string]any{"query": longChinese}, sdk.ToolRenderContext{}, 0)
	if err != nil {
		t.Fatalf("RenderSearchCall failed: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	line := lines[0]
	if !utf8.ValidString(line) {
		t.Fatalf("rendered line is not valid UTF-8: %s", line)
	}
	if !strings.HasSuffix(line, "...") {
		t.Fatalf("expected line to end with '...', got: %s", line)
	}
}
