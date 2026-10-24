package autocontinue

import "testing"

func floatPtr(v float64) *float64 { return &v }

func TestContinueHTTPStatuses(t *testing.T) {
	want := []float64{405, 408, 409, 421, 425, 429, 499, 500, 502, 503, 504, 520, 521, 522, 523, 524, 525, 529, 530}
	if len(ContinueHTTPStatuses) != len(want) {
		t.Fatalf("ContinueHTTPStatuses has %d entries, want %d", len(ContinueHTTPStatuses), len(want))
	}
	for _, status := range want {
		if !ContinueHTTPStatuses[status] {
			t.Fatalf("status %v missing from ContinueHTTPStatuses", status)
		}
	}
	for _, status := range []float64{400, 401, 403, 404, 413, 418, 422, 526, 528} {
		if ContinueHTTPStatuses[status] {
			t.Fatalf("status %v must not be continuable", status)
		}
	}
}

func TestExtractHTTPStatus(t *testing.T) {
	cases := []struct {
		text string
		want *float64
	}{
		{text: "HTTP 502", want: floatPtr(502)},
		{text: "http/1.1 503 Service Unavailable", want: floatPtr(503)},
		{text: "status: 429", want: floatPtr(429)},
		{text: "status_code=503", want: floatPtr(503)},
		{text: "status code 500", want: floatPtr(500)},
		{text: "error code 520", want: floatPtr(520)},
		{text: "error_code: 504", want: floatPtr(504)},
		{text: "code=405", want: floatPtr(405)},
		{text: "error#502", want: floatPtr(502)},
		// A bare three-digit number is not a status code.
		{text: "token limit 500 exceeded", want: nil},
		{text: "upstream returned 502", want: nil},
		{text: "port 8080 refused", want: nil},
		// Out-of-range or malformed values.
		{text: "HTTP 99", want: nil},
		{text: "HTTP 600", want: nil},
		{text: "", want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			got := ExtractHTTPStatus(tc.text)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("ExtractHTTPStatus(%q) = %v, want nil", tc.text, *got)
			case tc.want != nil && got == nil:
				t.Fatalf("ExtractHTTPStatus(%q) = nil, want %v", tc.text, *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Fatalf("ExtractHTTPStatus(%q) = %v, want %v", tc.text, *got, *tc.want)
			}
		})
	}
}

func TestIsContinueWorthyError(t *testing.T) {
	assistantError := func(errText string) map[string]any {
		return map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": errText}
	}
	cases := []struct {
		name       string
		message    map[string]any
		httpStatus *float64
		wantOK     bool
		wantReason string
	}{
		{name: "no message", message: nil, wantReason: "no-assistant"},
		{name: "not assistant", message: map[string]any{"role": "user"}, wantReason: "no-assistant"},
		{
			name:       "aborted stop",
			message:    map[string]any{"role": "assistant", "stopReason": "aborted"},
			wantReason: "aborted",
		},
		{
			name:       "length stop is not transient",
			message:    map[string]any{"role": "assistant", "stopReason": "length"},
			wantReason: "stop:length",
		},
		{
			name:       "missing stopReason",
			message:    map[string]any{"role": "assistant"},
			wantReason: "stop:none",
		},
		{name: "http 502 in text", message: assistantError("HTTP 502 bad gateway"), wantOK: true, wantReason: "http-502"},
		{name: "error code 429", message: assistantError("error code 429"), wantOK: true, wantReason: "http-429"},
		{
			name:       "recorded status wins over text",
			message:    assistantError("HTTP 502"),
			httpStatus: floatPtr(429),
			wantOK:     true,
			wantReason: "http-429",
		},
		{
			name:       "recorded 401 is non-retryable",
			message:    assistantError("HTTP 500"),
			httpStatus: floatPtr(401),
			wantReason: "non-retryable",
		},
		{
			name:       "recorded 403 is non-retryable",
			message:    assistantError("HTTP 500"),
			httpStatus: floatPtr(403),
			wantReason: "non-retryable",
		},
		{name: "401 in text", message: assistantError("HTTP 401 Unauthorized"), wantReason: "non-retryable"},
		{name: "skip pattern beats continuable status", message: assistantError("insufficient_quota (HTTP 503)"), wantReason: "non-retryable"},
		{name: "context overflow", message: assistantError("maximum context length exceeded"), wantReason: "non-retryable"},
		{name: "bare number is unmatched", message: assistantError("token limit 500 exceeded"), wantReason: "unmatched"},
		{name: "empty error text", message: assistantError(""), wantReason: "unmatched"},
		{name: "pattern match", message: assistantError("upstream connect error"), wantOK: true, wantReason: "pattern"},
		{name: "chinese relay busy", message: assistantError("无可用渠道"), wantOK: true, wantReason: "pattern"},
		{name: "socket hang up", message: assistantError("socket hang up"), wantOK: true, wantReason: "pattern"},
		{name: "plain text", message: assistantError("something odd happened"), wantReason: "unmatched"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsContinueWorthyError(tc.message, tc.httpStatus)
			if got.OK != tc.wantOK {
				t.Fatalf("ok = %v, want %v (reason %q)", got.OK, tc.wantOK, got.Reason)
			}
			if got.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", got.Reason, tc.wantReason)
			}
		})
	}
}

// TestContinueErrorPatternTransients covers the allowlist rows that do not need
// a status code; the keep-out list is covered by TestContinueErrorPatternExcludes.
func TestContinueErrorPatternTransients(t *testing.T) {
	transient := []string{
		"405 Method Not Allowed",
		"method not allowed",
		"请求方法不允许",
		"too many requests",
		"rate limit reached",
		"rate-limit exceeded",
		"provider overloaded",
		"resourceexhausted",
		"service unavailable",
		"bad gateway",
		"gateway timeout",
		"server error",
		"internal error",
		"provider returned error",
		"upstream failure",
		"渠道繁忙",
		"上游负载",
		"负载已饱和",
		"线路繁忙",
		"分组负载",
		"try again",
		"try your request again",
		"please retry",
		"you can retry",
		"retry delay 2s",
		"temporarily unavailable",
		"try again later",
		"network error",
		"connection refused",
		"connection reset by peer",
		"ECONNRESET",
		"ECONNREFUSED",
		"ENOTFOUND",
		"EAI_AGAIN",
		"ETIMEDOUT",
		"UND_ERR_CONNECT_TIMEOUT",
		"other side closed",
		"socket hang up",
		"socket connection was closed",
		"reset before headers",
		"fetch failed",
		"getaddrinfo ENOTFOUND",
		"timed out",
		"time out",
		"request timeout",
		"aborted due to timeout",
		"idle timeout",
		"headers timeout",
		"body timeout",
		"stream terminated",
		"websocket closed",
		"websocket error",
		"ended without response",
		"stream ended before message",
		"http2 request did not get a response",
		"unexpected token < in JSON",
		"unexpected end of json",
		"invalid json",
		"not valid json",
		"malformed json",
		"empty response",
		"no body received",
		"cloudflare error",
		"cf-ray: 8a1",
		"just a moment",
		"error code: 502",
	}
	for _, text := range transient {
		if !ContinueErrorPattern.MatchString(text) {
			t.Errorf("ContinueErrorPattern does not match %q", text)
		}
	}
}

func TestContinueErrorPatternExcludes(t *testing.T) {
	// 401/403/quota/overflow/truncation must never look transient.
	final := []string{
		"HTTP 401 Unauthorized",
		"HTTP 403 Forbidden",
		"insufficient_quota",
		"quota exceeded",
		"billing problem",
		"prompt is too long",
		"model not found",
		"invalid request",
		"content policy violation",
		"no error at all",
	}
	for _, text := range final {
		if ContinueErrorPattern.MatchString(text) {
			t.Errorf("ContinueErrorPattern unexpectedly matches %q", text)
		}
	}
}

func TestSkipErrorPattern(t *testing.T) {
	skipped := []string{
		"HTTP 401",
		"HTTP 403",
		"unauthorized",
		"forbidden",
		"invalid api key",
		"invalid_api_key",
		"incorrect api key",
		"authentication failed",
		"permission denied",
		"insufficient_quota",
		"insufficient quota",
		"out of budget",
		"quota exceeded",
		"billing",
		"GoUsageLimitError",
		"FreeUsageLimitError",
		"monthly usage limit reached",
		"余额不足",
		"额度不足",
		"令牌已过期",
		"token expired",
		"context length exceeded",
		"context_overflow",
		"prompt too long",
		"maximum context",
		"please reduce",
		"content filter",
		"content_policy",
		"safety",
		"moderation",
		"model not found",
		"unknown model",
		"invalid request",
		"unprocessable",
	}
	for _, text := range skipped {
		if !SkipErrorPattern.MatchString(text) {
			t.Errorf("SkipErrorPattern does not match %q", text)
		}
	}
	if SkipErrorPattern.MatchString("connection reset") {
		t.Error("SkipErrorPattern matched a transient error")
	}
}
