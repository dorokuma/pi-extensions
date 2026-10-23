// Package pitinyfish implements the TinyFish search and fetch extension for PiG.
package pitinyfish

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

const (
	// SearchTimeoutMS is the default client timeout for search (10,000ms).
	SearchTimeoutMS = 10_000

	// FetchTimeoutMS is the default client timeout for fetch (150,000ms).
	FetchTimeoutMS = 150_000

	// MaxToolBytes is the maximum allowed byte threshold for truncation override.
	MaxToolBytes = 200_000

	// MaxFetchURLs is the maximum number of URLs allowed per fetch request.
	MaxFetchURLs = 10

	// LinkPreviewLimit is the maximum number of links to enumerate in formatted fetch output.
	LinkPreviewLimit = 50
)

// TruncationDetails contains output truncation metrics.
type TruncationDetails struct {
	Truncated   bool   `json:"truncated"`
	TruncatedBy string `json:"truncatedBy"`
	TotalLines  int    `json:"totalLines"`
	TotalBytes  int    `json:"totalBytes"`
	OutputLines int    `json:"outputLines"`
	OutputBytes int    `json:"outputBytes"`
}

// TruncationOutput wraps the truncated text along with its details.
type TruncationOutput struct {
	Text    string            `json:"text"`
	Details TruncationDetails `json:"details"`
}

// SearchFormatOptions specifies optional metadata for formatting search output.
type SearchFormatOptions struct {
	Location string
	Language string
}

// NormalizeNumber parses and clamps a numeric input value to [minVal, maxVal].
func NormalizeNumber(val any, fallback, minVal, maxVal int) int {
	if val == nil {
		return fallback
	}
	var num float64
	switch v := val.(type) {
	case int:
		num = float64(v)
	case int64:
		num = float64(v)
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fallback
		}
		num = v
	default:
		return fallback
	}
	floor := int(math.Floor(num))
	if floor < minVal {
		return minVal
	}
	if floor > maxVal {
		return maxVal
	}
	return floor
}

// NormalizeUrls extracts, trims, filters empty strings, and deduplicates URLs from url and urls parameters.
func NormalizeUrls(rawURL any, rawURLs any) []string {
	var raw []string
	if u, ok := rawURL.(string); ok && u != "" {
		raw = append(raw, u)
	}
	if list, ok := rawURLs.([]any); ok {
		for _, item := range list {
			if s, ok := item.(string); ok {
				raw = append(raw, s)
			}
		}
	} else if list, ok := rawURLs.([]string); ok {
		raw = append(raw, list...)
	}

	seen := make(map[string]bool)
	var urls []string
	for _, val := range raw {
		trimmed := strings.TrimSpace(val)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		urls = append(urls, trimmed)
	}
	return urls
}

// NormalizeFetchFormat ensures format is one of "markdown", "html", or "json".
func NormalizeFetchFormat(val any) string {
	s, ok := val.(string)
	if !ok {
		return ""
	}
	s = strings.TrimSpace(s)
	switch s {
	case "markdown", "html", "json":
		return s
	default:
		return ""
	}
}

// TruncateToolText truncates formatted tool output using head truncation, mirroring index.ts truncateToolText.
func TruncateToolText(text string, maxBytes int) TruncationOutput {
	if maxBytes <= 0 {
		maxBytes = sdk.DefaultMaxBytes
	}
	maxLines := sdk.DefaultMaxLines
	res := sdk.TruncateHead(text, sdk.TruncationOptions{
		MaxBytes: &maxBytes,
		MaxLines: &maxLines,
	})

	details := TruncationDetails{
		Truncated:   res.Truncated,
		TruncatedBy: res.TruncatedBy,
		TotalLines:  res.TotalLines,
		TotalBytes:  res.TotalBytes,
		OutputLines: res.OutputLines,
		OutputBytes: res.OutputBytes,
	}

	if !res.Truncated {
		return TruncationOutput{
			Text:    res.Content,
			Details: details,
		}
	}

	notice := fmt.Sprintf("\n\n[Output truncated: %d of %d lines (%s of %s). Use maxBytes to raise the limit, or narrow the request.]",
		res.OutputLines, res.TotalLines, sdk.FormatSize(res.OutputBytes), sdk.FormatSize(res.TotalBytes))

	return TruncationOutput{
		Text:    res.Content + notice,
		Details: details,
	}
}

// FormatSearchResponse formats a SearchResponse into Markdown, matching index.ts formatSearchResponse.
func FormatSearchResponse(res *SearchResponse, opts SearchFormatOptions) string {
	var lines []string
	lines = append(lines,
		"# TinyFish Search",
		fmt.Sprintf("Query: %s", res.Query),
		fmt.Sprintf("Page: %d", res.Page),
		fmt.Sprintf("Total results: %d", res.TotalResults),
	)
	if opts.Location != "" {
		lines = append(lines, fmt.Sprintf("Location: %s", opts.Location))
	}
	if opts.Language != "" {
		lines = append(lines, fmt.Sprintf("Language: %s", opts.Language))
	}
	lines = append(lines, "")

	if len(res.Results) == 0 {
		lines = append(lines, "No results returned.")
		return strings.Join(lines, "\n")
	}

	for _, item := range res.Results {
		lines = append(lines, fmt.Sprintf("## %d. %s", item.Position, item.Title))
		lines = append(lines, fmt.Sprintf("URL: %s", item.URL))
		lines = append(lines, fmt.Sprintf("Site: %s", item.SiteName))
		if item.Snippet != "" {
			lines = append(lines, fmt.Sprintf("Snippet: %s", item.Snippet))
		}
		lines = append(lines, "")
	}

	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// StringifyFetchText serializes page content text according to its type.
func StringifyFetchText(text any) string {
	if text == nil {
		return ""
	}
	if s, ok := text.(string); ok {
		return s
	}
	b, err := json.MarshalIndent(text, "", "  ")
	if err != nil {
		return fmt.Sprint(text)
	}
	return string(b)
}

func appendLinks(lines []string, title string, links []string) []string {
	if len(links) == 0 {
		return lines
	}
	lines = append(lines, fmt.Sprintf("%s: %d", title, len(links)))
	limit := LinkPreviewLimit
	if len(links) < limit {
		limit = len(links)
	}
	for i := 0; i < limit; i++ {
		lines = append(lines, fmt.Sprintf("- %s", links[i]))
	}
	if len(links) > LinkPreviewLimit {
		lines = append(lines, fmt.Sprintf("- ... %d more", len(links)-LinkPreviewLimit))
	}
	return lines
}

// FormatFetchResponse formats a FetchResponse into Markdown, matching index.ts formatFetchResponse.
func FormatFetchResponse(res *FetchResponse) string {
	var lines []string
	lines = append(lines,
		"# TinyFish Fetch",
		fmt.Sprintf("Successful: %d", len(res.Results)),
		fmt.Sprintf("Errors: %d", len(res.Errors)),
		"",
	)

	for i, page := range res.Results {
		title := page.URL
		if page.Title != nil && *page.Title != "" {
			title = *page.Title
		}
		text := StringifyFetchText(page.Text)

		lines = append(lines, fmt.Sprintf("## %d. %s", i+1, title))
		lines = append(lines, fmt.Sprintf("URL: %s", page.URL))
		if page.FinalURL != nil && *page.FinalURL != "" && *page.FinalURL != page.URL {
			lines = append(lines, fmt.Sprintf("Final URL: %s", *page.FinalURL))
		}
		if page.Description != nil && *page.Description != "" {
			lines = append(lines, fmt.Sprintf("Description: %s", *page.Description))
		}
		if page.Language != nil && *page.Language != "" {
			lines = append(lines, fmt.Sprintf("Language: %s", *page.Language))
		}
		if page.Author != nil && *page.Author != "" {
			lines = append(lines, fmt.Sprintf("Author: %s", *page.Author))
		}
		if page.PublishedDate != nil && *page.PublishedDate != "" {
			lines = append(lines, fmt.Sprintf("Published: %s", *page.PublishedDate))
		}
		if page.LatencyMs != nil {
			lines = append(lines, fmt.Sprintf("Latency: %dms", *page.LatencyMs))
		}
		lines = append(lines, fmt.Sprintf("Format: %s", page.Format))

		lines = appendLinks(lines, "Links", page.Links)
		lines = appendLinks(lines, "Image links", page.ImageLinks)

		lines = append(lines, "", "### Content")
		if page.Format == "html" {
			if text == "" {
				text = "(empty)"
			}
			lines = append(lines, "```html", text, "```")
		} else if page.Format == "json" {
			if text == "" {
				text = "null"
			}
			lines = append(lines, "```json", text, "```")
		} else {
			if text == "" {
				text = "(empty)"
			}
			lines = append(lines, text)
		}
		lines = append(lines, "")
	}

	if len(res.Errors) > 0 {
		lines = append(lines, "## Errors")
		for _, item := range res.Errors {
			lines = append(lines, fmt.Sprintf("- %s: %s", item.URL, item.Error))
		}
	}

	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// textResult creates a standard sdk.ToolResult.
func textResult(text string, details map[string]any) sdk.ToolResult {
	return sdk.ToolResult{
		Content: text,
		Details: details,
	}
}

// clientFactory is an internal seam to allow injecting mock clients in tests.
var clientFactory = NewClient

// setClientFactory overrides the client factory function for tests only (测试注入用).
func setClientFactory(f func(opts ...ClientOption) (*Client, error)) func() {
	orig := clientFactory
	clientFactory = f
	return func() { clientFactory = orig }
}

func toContext(ctx sdk.Context) (context.Context, context.CancelFunc) {
	reqCtx, cancel := context.WithCancel(context.Background())
	if doneCh := ctx.Done(); doneCh != nil {
		go func() {
			select {
			case <-doneCh:
				cancel()
			case <-reqCtx.Done():
			}
		}()
	}
	return reqCtx, cancel
}

func executeSearch(ctx sdk.Context, params map[string]any) (any, error) {
	queryRaw, _ := params["query"].(string)
	query := strings.TrimSpace(queryRaw)
	if query == "" {
		return textResult("Error: No query provided.", map[string]any{"error": "No query provided."}), nil
	}

	var pagePtr *int
	if pageVal, ok := params["page"]; ok && pageVal != nil {
		switch pageVal.(type) {
		case int, int64, float64:
			clamped := NormalizeNumber(pageVal, 0, 0, 10)
			pagePtr = &clamped
		}
	}

	location, _ := params["location"].(string)
	language, _ := params["language"].(string)

	maxBytes := NormalizeNumber(params["maxBytes"], sdk.DefaultMaxBytes, 1000, MaxToolBytes)
	timeoutMs := NormalizeNumber(params["timeoutMs"], SearchTimeoutMS, 1000, 120_000)

	_ = ctx.OnUpdate(map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": fmt.Sprintf("Searching TinyFish: %s", query)},
		},
		"details": map[string]any{"phase": "search", "query": query},
	})

	client, err := clientFactory(WithTimeout(time.Duration(timeoutMs) * time.Millisecond))
	if err != nil {
		msg := err.Error()
		return textResult(fmt.Sprintf("Error: %s", msg), map[string]any{
			"provider": "tinyfish",
			"kind":     "search",
			"query":    query,
			"error":    msg,
		}), nil
	}

	searchParams := SearchParams{
		Query:    query,
		Location: strings.TrimSpace(location),
		Language: strings.TrimSpace(language),
		Page:     pagePtr,
	}

	reqCtx, cancel := toContext(ctx)
	defer cancel()

	resp, err := client.Search(reqCtx, searchParams)
	if err != nil {
		msg := err.Error()
		return textResult(fmt.Sprintf("Error: %s", msg), map[string]any{
			"provider": "tinyfish",
			"kind":     "search",
			"query":    query,
			"error":    msg,
		}), nil
	}

	formatted := FormatSearchResponse(resp, SearchFormatOptions{
		Location: searchParams.Location,
		Language: searchParams.Language,
	})
	output := TruncateToolText(formatted, maxBytes)

	return textResult(output.Text, map[string]any{
		"provider":        "tinyfish",
		"kind":            "search",
		"query":           resp.Query,
		"page":            resp.Page,
		"totalResults":    resp.TotalResults,
		"returnedResults": len(resp.Results),
		"truncation":      output.Details,
		"results":         resp.Results,
	}), nil
}

func executeFetch(ctx sdk.Context, params map[string]any) (any, error) {
	urls := NormalizeUrls(params["url"], params["urls"])
	if len(urls) == 0 {
		return textResult("Error: No URL provided.", map[string]any{"error": "No URL provided."}), nil
	}
	if len(urls) > MaxFetchURLs {
		msg := fmt.Sprintf("TinyFish Fetch accepts at most %d URLs per request.", MaxFetchURLs)
		return textResult(fmt.Sprintf("Error: %s", msg), map[string]any{"error": msg}), nil
	}

	format := NormalizeFetchFormat(params["format"])

	var linksPtr *bool
	if l, ok := params["links"].(bool); ok {
		linksPtr = &l
	}
	var imgLinksPtr *bool
	if il, ok := params["image_links"].(bool); ok {
		imgLinksPtr = &il
	}

	maxBytes := NormalizeNumber(params["maxBytes"], sdk.DefaultMaxBytes, 1000, MaxToolBytes)
	timeoutMs := NormalizeNumber(params["timeoutMs"], FetchTimeoutMS, 1000, 180_000)

	_ = ctx.OnUpdate(map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": fmt.Sprintf("Fetching %d URL(s) with TinyFish...", len(urls))},
		},
		"details": map[string]any{"phase": "fetch", "urlCount": len(urls)},
	})

	client, err := clientFactory(WithTimeout(time.Duration(timeoutMs) * time.Millisecond))
	if err != nil {
		msg := err.Error()
		return textResult(fmt.Sprintf("Error: %s", msg), map[string]any{
			"provider": "tinyfish",
			"kind":     "fetch",
			"urls":     urls,
			"error":    msg,
		}), nil
	}

	fetchParams := FetchParams{
		URLs:       urls,
		Format:     format,
		Links:      linksPtr,
		ImageLinks: imgLinksPtr,
	}

	reqCtx, cancel := toContext(ctx)
	defer cancel()

	resp, err := client.Fetch(reqCtx, fetchParams)
	if err != nil {
		msg := err.Error()
		return textResult(fmt.Sprintf("Error: %s", msg), map[string]any{
			"provider": "tinyfish",
			"kind":     "fetch",
			"urls":     urls,
			"error":    msg,
		}), nil
	}

	formatted := FormatFetchResponse(resp)
	output := TruncateToolText(formatted, maxBytes)

	resultsSummaries := make([]map[string]any, 0, len(resp.Results))
	for _, page := range resp.Results {
		textLen := len(StringifyFetchText(page.Text))
		var linksCount any
		if page.Links != nil {
			linksCount = len(page.Links)
		}
		var imgLinksCount any
		if page.ImageLinks != nil {
			imgLinksCount = len(page.ImageLinks)
		}
		summary := map[string]any{
			"url":             page.URL,
			"final_url":       page.FinalURL,
			"title":           page.Title,
			"description":     page.Description,
			"language":        page.Language,
			"author":          page.Author,
			"published_date":  page.PublishedDate,
			"format":          page.Format,
			"textLength":      textLen,
			"linksCount":      linksCount,
			"imageLinksCount": imgLinksCount,
			"latency_ms":      page.LatencyMs,
		}
		resultsSummaries = append(resultsSummaries, summary)
	}

	return textResult(output.Text, map[string]any{
		"provider":   "tinyfish",
		"kind":       "fetch",
		"urlCount":   len(urls),
		"successful": len(resp.Results),
		"errors":     resp.Errors,
		"truncation": output.Details,
		"results":    resultsSummaries,
	}), nil
}

// ExecuteSearch executes the tinyfish_search tool.
func ExecuteSearch(ctx sdk.Context, params map[string]any) (any, error) {
	return executeSearch(ctx, params)
}

// ExecuteFetch executes the tinyfish_fetch tool.
func ExecuteFetch(ctx sdk.Context, params map[string]any) (any, error) {
	return executeFetch(ctx, params)
}

// SearchToolDefinition returns the full tool definition for tinyfish_search.
func SearchToolDefinition() sdk.ToolDefinition {
	return sdk.ToolDefinition{
		Name:  "tinyfish_search",
		Label: "TinyFish Search",
		Description: "Search the web using the TinyFish Search API via @tiny-fish/sdk. Requires TINYFISH_API_KEY. " +
			"Returns ranked titles, snippets, and URLs. Output is truncated to 50KB by default.",
		PromptSnippet: "Search the web with TinyFish for ranked results, snippets, and URLs. Use location/language for geo-targeting.",
		PromptGuidelines: []string{
			"ALWAYS use tinyfish_search for web searches. NEVER use bash curl/wget. Only exception: localhost testing and downloading binary files.",
			"Use tinyfish_search when the user needs web search results or URLs before fetching full page content.",
			"Use search operators in tinyfish_search query strings, such as site:docs.example.com or -site:youtube.com, when scoping results helps.",
		},
		Parameters: sdk.Schema{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"minLength":   1,
					"description": "Search query string. Supports search operators like site: and -site:.",
				},
				"location": map[string]any{
					"type":        "string",
					"description": "Country code for localized results, e.g. US, GB, FR.",
				},
				"language": map[string]any{
					"type":        "string",
					"description": "Language code for results, e.g. en, fr, de.",
				},
				"page": map[string]any{
					"type":        "number",
					"minimum":     0,
					"maximum":     10,
					"description": "Search result page number, starting at 0 (max 10).",
				},
				"maxBytes": map[string]any{
					"type":        "number",
					"minimum":     1000,
					"maximum":     MaxToolBytes,
					"description": "Maximum bytes to return to the model (default 50KB, max 200KB).",
				},
				"timeoutMs": map[string]any{
					"type":        "number",
					"minimum":     1000,
					"maximum":     120000,
					"description": "Client timeout in milliseconds (default 10000).",
				},
			},
			"required": []string{"query"},
		},
		Execute:      ExecuteSearch,
		RenderCall:   RenderSearchCall,
		RenderResult: RenderSearchResult,
	}
}

// FetchToolDefinition returns the full tool definition for tinyfish_fetch.
func FetchToolDefinition() sdk.ToolDefinition {
	return sdk.ToolDefinition{
		Name:  "tinyfish_fetch",
		Label: "TinyFish Fetch",
		Description: "Fetch and extract clean content from up to 10 URLs using the TinyFish Fetch API via @tiny-fish/sdk. " +
			"Requires TINYFISH_API_KEY. Use markdown by default; html/json are available. Output is truncated to 50KB by default. " +
			"TinyFish recommends a 150s client timeout for fetch.",
		PromptSnippet: "Fetch known URLs with TinyFish to get rendered/extracted page content as markdown, html, or json.",
		PromptGuidelines: []string{
			"ALWAYS use tinyfish_fetch for fetching web content. NEVER use bash curl/wget. Only exception: localhost testing and downloading binary files.",
			"Use tinyfish_fetch when you already have URLs and need full extracted content; use tinyfish_search first if you need to discover URLs.",
			fmt.Sprintf("tinyfish_fetch accepts at most %d URLs per call; batch related URLs together when useful.", MaxFetchURLs),
		},
		Parameters: sdk.Schema{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "Single URL to fetch.",
				},
				"urls": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"minItems":    1,
					"maxItems":    MaxFetchURLs,
					"description": "URLs to fetch. TinyFish accepts up to 10 URLs per request.",
				},
				"format": map[string]any{
					"type":        "string",
					"enum":        []string{"markdown", "html", "json"},
					"description": "Output format for extracted content (default markdown).",
				},
				"links": map[string]any{
					"type":        "boolean",
					"description": "Include page hyperlinks in each result.",
				},
				"image_links": map[string]any{
					"type":        "boolean",
					"description": "Include image URLs in each result.",
				},
				"maxBytes": map[string]any{
					"type":        "number",
					"minimum":     1000,
					"maximum":     MaxToolBytes,
					"description": "Maximum bytes to return to the model (default 50KB, max 200KB).",
				},
				"timeoutMs": map[string]any{
					"type":        "number",
					"minimum":     1000,
					"maximum":     180000,
					"description": "Client timeout in milliseconds (default 150000).",
				},
			},
		},
		Execute:      ExecuteFetch,
		RenderCall:   RenderFetchCall,
		RenderResult: RenderFetchResult,
	}
}

// Extension creates and registers the pi-tinyfish extension tools.
func Extension() *sdk.Extension {
	e := sdk.New("pi-tinyfish")
	e.RegisterTool(SearchToolDefinition())
	e.RegisterTool(FetchToolDefinition())
	return e
}
