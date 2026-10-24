package autocontinue

import (
	"fmt"
	"regexp"
)

// ContinueHTTPStatuses mirrors TS CONTINUE_HTTP_STATUSES
// (auto-continue.ts:146-171): HTTP statuses that are usually safe to retry or
// continue. Membership is keyed by the JS number the event carries, so the
// map is keyed by float64.
var ContinueHTTPStatuses = map[float64]bool{
	405: true, // 中转站 / nginx / WAF misreporting Method Not Allowed
	408: true, // Request Timeout
	409: true, // some gateways use conflict for an upstream lock / transient clash
	421: true, // Misdirected Request
	425: true, // Too Early
	429: true, // still worth another round after the built-in retry gives up
	499: true, // nginx Client Closed Request
	500: true,
	502: true,
	503: true,
	504: true,
	520: true, // Cloudflare: unknown
	521: true, // web server down
	522: true, // connection timed out
	523: true, // origin unreachable
	524: true, // timeout
	525: true, // SSL handshake
	529: true, // site overloaded
	530: true,
}

// mustPattern compiles a pattern built from JS-compatible fragments.
func mustPattern(parts ...string) *regexp.Regexp {
	joined := ""
	for i, part := range parts {
		if i > 0 {
			joined += "|"
		}
		joined += part
	}
	return regexp.MustCompile(`(?i)` + joined)
}

// ContinueErrorPattern mirrors TS CONTINUE_ERROR_PATTERN
// (auto-continue.ts:172-244, case-insensitive). Errors that look transient even
// without a clean status code. It stays an allowlist: 401/403/quota/overflow
// must not match.
//
// Go's regexp has no non-capturing groups, so the TS `(?:...)` groups are
// written as capturing ones; only MatchString is used, so the groups are inert.
var ContinueErrorPattern = mustPattern(
	// Three-digit statuses only count behind an HTTP/status/code/error prefix.
	// A bare number does not: "token limit 500 exceeded" must stay unmatched.
	`(http([/`+jsSpaceBody+`]?[0-9]([.][0-9])?)?|status([`+jsSpaceBody+`_]*code)?|error[`+jsSpaceBody+`_]*code|code|error)[`+jsSpaceBody+`:=#_-]*(405|408|409|429|499|50[0-4]|52[0-5]|529)\b`,
	`method not allowed`,
	`请求方法不允许`,
	`too many requests`,
	`rate`+jsDotClass+`?limit`,
	`overloaded`,
	`resourceexhausted`,
	`service`+jsDotClass+`?unavailable`,
	`bad gateway`,
	`gateway time-?out`,
	`server`+jsDotClass+`?error`,
	`internal`+jsDotClass+`?error`,
	`provider`+jsDotClass+`?returned`+jsDotClass+`?error`,
	`upstream`,
	`无可用渠道`,
	`渠道繁忙`,
	`上游负载`,
	`负载已饱和`,
	`线路繁忙`,
	`分组负载`,
	`try (your request )?again`,
	`please retry`,
	`you can retry`,
	`retry delay`,
	`temporarily unavailable`,
	`try again later`,
	`network`+jsDotClass+`?error`,
	`connection`+jsDotClass+`?error`,
	`connection`+jsDotClass+`?refused`,
	`connection`+jsDotClass+`?lost`,
	`connection reset`,
	`econnreset`,
	`econnrefused`,
	`enotfound`,
	`eai_again`,
	`etimedout`,
	`und_err_`,
	`other side closed`,
	`socket hang up`,
	`socket connection was closed`,
	`reset before headers`,
	`fetch failed`,
	`getaddrinfo`,
	`timed? ?out`,
	`timeout`,
	`aborted due to timeout`,
	`idle timeout`,
	`headers timeout`,
	`body timeout`,
	`terminated`,
	`websocket`+jsDotClass+`?closed`,
	`websocket`+jsDotClass+`?error`,
	`ended without`,
	`stream ended before`,
	`http2 request did not get a response`,
	`unexpected token`+jsSpaceClass+`*<`,
	`unexpected end of json`,
	`invalid json`,
	`not valid json`,
	`malformed json`,
	`empty response`,
	`no body`,
	`cloudflare`,
	`cf-ray`,
	`just a moment`,
	`error code:`+jsSpaceClass+`*5[0-9][0-9]`,
)

// SkipErrorPattern mirrors TS SKIP_ERROR_PATTERN (auto-continue.ts:246-283,
// case-insensitive). A match ends the decision: no status code reopens it.
var SkipErrorPattern = mustPattern(
	`\b401\b`,
	`\b403\b`,
	`unauthorized`,
	`forbidden`,
	`invalid`+jsDotClass+`?api`+jsDotClass+`?key`,
	`incorrect api key`,
	`authentication`,
	`permission denied`,
	`insufficient_quota`,
	`insufficient`+jsDotClass+`?quota`,
	`out of budget`,
	`quota exceeded`,
	`billing`,
	`gousagelimiterror`,
	`freeusagelimiterror`,
	`monthly usage limit`,
	`余额不足`,
	`额度不足`,
	`令牌已过期`,
	`token expired`,
	`context`+jsDotClass+`?length`,
	`context`+jsDotClass+`?overflow`,
	`prompt`+jsDotClass+`?too`+jsDotClass+`?long`,
	`maximum context`,
	`please reduce`,
	`content`+jsDotClass+`?filter`,
	`content`+jsDotClass+`?policy`,
	`safety`,
	`moderation`,
	`model`+jsDotClass+`?not`+jsDotClass+`?found`,
	`unknown model`,
	`invalid`+jsDotClass+`?request`,
	`unprocessable`,
)

// reHTTPStatus mirrors TS extractHttpStatus's regex (auto-continue.ts:513-515).
var reHTTPStatus = regexp.MustCompile(`(?i)\b(http([/` + jsSpaceBody + `]?[0-9]([.][0-9])?)?|status([` + jsSpaceBody + `_]*code)?|error[` + jsSpaceBody + `_]*code|code|error)[` + jsSpaceBody + `:=#_-]*([1-5][0-9][0-9])\b`)

// ExtractHTTPStatus mirrors TS extractHttpStatus (auto-continue.ts:512): an HTTP
// status code only when the text carries an explicit HTTP / status / code /
// error prefix. A bare three-digit number (token counts, ports, byte sizes) is
// not a status code.
func ExtractHTTPStatus(text string) *float64 {
	match := reHTTPStatus.FindStringSubmatch(text)
	if match == nil {
		return nil
	}
	value, ok := jsNumber(match[len(match)-1])
	if !ok {
		return nil
	}
	return &value
}

// Verdict is the shared ok/reason pair TS uses for both continuation gates.
type Verdict struct {
	OK     bool
	Reason string
}

func verdict(ok bool, reason string) Verdict { return Verdict{OK: ok, Reason: reason} }

// stopReasonOr mirrors TS `message.stopReason ?? "none"`.
func stopReasonOr(message map[string]any) string {
	reason, _ := message["stopReason"].(string)
	if reason == "" {
		return "none"
	}
	return reason
}

// roleOf returns a message's role, or "" when it is not a string.
func roleOf(message map[string]any) string {
	role, _ := message["role"].(string)
	return role
}

// stopReasonOf returns a message's stopReason, or "" when it is not a string.
func stopReasonOf(message map[string]any) string {
	reason, _ := message["stopReason"].(string)
	return reason
}

// IsContinueWorthyError mirrors TS isContinueWorthyError
// (auto-continue.ts:521-556). httpStatus is the status seen on the provider
// response (nil when none was recorded); it takes precedence over the status
// parsed out of the error text, as TS's `httpStatus ?? extractHttpStatus(err)`.
func IsContinueWorthyError(message map[string]any, httpStatus *float64) Verdict {
	if message == nil || roleOf(message) != "assistant" {
		return verdict(false, "no-assistant")
	}
	switch stopReasonOf(message) {
	case "aborted":
		return verdict(false, "aborted")
	case "error":
		// keep going
	default:
		// `length` lands here too: truncation is not a network hiccup, so no
		// automatic continue (and no transient prompt).
		return verdict(false, "stop:"+stopReasonOr(message))
	}

	errText := jsTrim(jsStringOf(message["errorMessage"]))
	status := httpStatus
	if status == nil {
		status = ExtractHTTPStatus(errText)
	}

	// 401/403 are authentication/permission problems: continuing cannot help.
	if status != nil && (*status == 401 || *status == 403) {
		return verdict(false, "non-retryable")
	}

	// A SKIP match is final: no status code reopens the decision.
	if errText != "" && SkipErrorPattern.MatchString(errText) {
		return verdict(false, "non-retryable")
	}

	if status != nil && ContinueHTTPStatuses[*status] {
		return verdict(true, fmt.Sprintf("http-%s", formatJSNumber(*status)))
	}
	if errText != "" && ContinueErrorPattern.MatchString(errText) {
		return verdict(true, "pattern")
	}
	return verdict(false, "unmatched")
}
