package herdsman

// Port of test/unit/upstream-error.test.ts. The must-match / must-not-match
// tables are copied case for case from the TS test.each lists.

import (
	"strings"
	"testing"
)

func textRef(value string) *string { return &value }

func TestUpstreamErrorMatchesRealErrorShapes(t *testing.T) {
	positives := []string{
		"API Error: 429 Rate limit exceeded",
		`{"type":"error","error":{"type":"rate_limit_error","message":"Rate limit reached"}}`,
		"rate_limit_error",
		"overloaded_error",
		"resource_exhausted",
		"ECONNRESET",
		"ETIMEDOUT",
		"ENOTFOUND",
		"EAI_AGAIN",
		"read ECONNRESET",
		"socket hang up",
		"fetch failed",
		"und_err_123",
		"request timed out after 60s",
		"timeout of 30000ms exceeded",
		"connection error: other side closed",
		"the model is overloaded, please retry",
		"the model is currently overloaded, please try again",
		"you exceeded your current quota, please check your plan and billing details",
		"insufficient_quota",
		"quota exceeded",
		"频率限制",
		"请求过于频繁",
		"模型过载",
		"请求过于频繁，请稍后重试",
		"资源耗尽",
		"Rate limit reached for gpt-4 in organization org-x on tokens per minute (TPM)",
		"Rate limit hit. Please slow down.",
		"The request failed with status 503",
		"error 429",
		"error: 429",
		"error status 429",
		"error code 503",
		"err 429",
		"failed 503",
		"failed with status 503",
		"failed with status code 503",
		"Error 429: rate limited",
		"Request failed with status code 503",
		"频率限制，请稍后重试",
		"模型过载，请稍候。",
		"429 Too Many Requests",
		"503 Service Unavailable",
		"529 overloaded",
		"ECONNREFUSED",
		"connect ECONNREFUSED",
	}
	for _, text := range positives {
		t.Run(text, func(t *testing.T) {
			match := MatchUpstreamModelError(textRef(text), DefaultWakeFilterConfig)
			if !match.Matched {
				t.Fatalf("expected a match for %q", text)
			}
			if match.Pattern == "" {
				t.Fatalf("matched without a pattern name for %q", text)
			}
			if !IsUpstreamModelError(textRef(text), DefaultWakeFilterConfig) {
				t.Fatalf("isUpstreamModelError disagrees for %q", text)
			}
		})
	}
}

func TestUpstreamErrorDoesNotMisclassifyOrdinaryProse(t *testing.T) {
	negatives := []string{
		"HTTP 429 状态码代表客户端发送的请求过多（Too Many Requests）。",
		"查过了，日志里一共发现了 3 次 503 错误。",
		"Fix completed. I updated the rate limit logic in the config.",
		"已修复：我们在客户端请求中增加了重试，避免因为频率限制导致失败。",
		"Connection error handling has been implemented in network.ts.",
		"Please try again later if the pane is still starting.",
		"Done. Added HTTP 503 retry in the client.",
		"Updated tokens per minute documentation.",
		"Implemented fetch failed fallback in transport.ts.",
		"已说明频率限制与配额的区别。",
		"Server error, please try again later",
		"429",
		"503",
		"529",
		"timeout",
		"error",
		"please try again later",
		"Documented status 429 handling.",
		"The provider returned status 429 once.",
		"See code 429 in the table.",
		"The response code was 503.",
		"Documented error 429 handling.",
		"Fixed error handling for 429.",
		"Done. Added error 503 retry in the client.",
		"Fix complete. The error handler now treats 429 as retryable.",
		"The build failed. 503 was only mentioned in comments.",
		"I preferred 429 as the test fixture.",
		"No error occurred; the 429 was expected.",
		"We handled the error, then documented 429.",
		"Documented error handling for 429.",
		"The test failed; we now cover 429.",
		"interrupt 429",
		"Ferrari 429",
		"The error message said 429 so I documented it.",
		"failed the test because 503 was mocked",
		"频率限制与配额的区别已说明。",
		"频率限制已修复。",
		"频率限制的处理已加到配置里。",
		"请求过于频繁的场景我们已在文档中说明。",
		"资源耗尽的配置项已修复。",
		"频率限制以及重试间隔需要产品确认。",
		"请求过于频繁是常见现象。",
		"资源耗尽也被写进了文档。",
		"模型过载了需要扩容。",
		"模型过载在高峰期出现。",
		"Error 429 was documented in the README.",
		"Failed 503 times in the test suite.",
		"error with 503 retries configured.",
	}
	for _, text := range negatives {
		t.Run(text, func(t *testing.T) {
			if IsUpstreamModelError(textRef(text), DefaultWakeFilterConfig) {
				t.Fatalf("misclassified %q as an upstream error", text)
			}
			if match := MatchUpstreamModelError(textRef(text), DefaultWakeFilterConfig); match.Matched {
				t.Fatalf("matchUpstreamModelError matched %q via %s", text, match.Pattern)
			}
		})
	}
}

func TestUpstreamErrorIgnoresEmptyInput(t *testing.T) {
	for _, text := range []string{"", "   ", "\t", "\n"} {
		if IsUpstreamModelError(textRef(text), DefaultWakeFilterConfig) {
			t.Fatalf("empty-ish input %q matched", text)
		}
		if match := MatchUpstreamModelError(textRef(text), DefaultWakeFilterConfig); match.Matched {
			t.Fatalf("empty-ish input %q matched a pattern", text)
		}
	}
}

func TestUpstreamErrorIgnoresLongSubstantiveReport(t *testing.T) {
	report := strings.Repeat("Chapter about the implementation. ", 30) +
		" The run reported a 429 from the provider once, and the retry logic also handles timeout and rate limit paths. " +
		strings.Repeat("More prose. ", 40)
	if len(report) <= MaxErrorShapedChars {
		t.Fatalf("report must exceed the error-shaped window, got %d", len(report))
	}
	if isErrorShaped(report) {
		t.Fatal("long report is error-shaped")
	}
	if IsUpstreamModelError(textRef(report), DefaultWakeFilterConfig) {
		t.Fatal("long report matched")
	}
}

func TestUpstreamErrorIgnoresHeadingOrFencedLongText(t *testing.T) {
	fenced := "# Report\n" + strings.Repeat("detail ", 80) + "\n```\napi error: 429\n```"
	if IsUpstreamModelError(textRef(fenced), DefaultWakeFilterConfig) {
		t.Fatal("fenced report matched")
	}
	codeOnly := "api error: 429\n```\nretry\n```"
	if !isErrorShaped(codeOnly) {
		t.Fatal("code-only text should still be error-shaped")
	}
	if IsUpstreamModelError(textRef(codeOnly), DefaultWakeFilterConfig) {
		t.Fatal("substantive fenced text matched")
	}
}

func TestUpstreamErrorIgnoresShortTextWithCodeFence(t *testing.T) {
	if IsUpstreamModelError(textRef("API Error: 429 rate_limit_error ```"), DefaultWakeFilterConfig) {
		t.Fatal("fenced short text matched")
	}
}

func TestUpstreamErrorRespectsEnabledSwitch(t *testing.T) {
	disabled := WakeFilterConfig{Enabled: false, ExtraPatterns: []string{}}
	if IsUpstreamModelError(textRef("API Error: 429 rate_limit_error"), disabled) {
		t.Fatal("disabled filter matched")
	}
	if IsUpstreamModelError(textRef("read ECONNRESET"), disabled) {
		t.Fatal("disabled filter matched")
	}
	if !IsUpstreamModelError(textRef("API Error: 429 rate_limit_error"), DefaultWakeFilterConfig) {
		t.Fatal("default filter did not match")
	}
}

func TestUpstreamErrorMatchesCustomSubstringCaseInsensitively(t *testing.T) {
	config := WakeFilterConfig{Enabled: true, ExtraPatterns: []string{"CHECKPOINT-STALL"}}
	if !IsUpstreamModelError(textRef("warning: checkpoint-stall detected"), config) {
		t.Fatal("custom substring did not match")
	}
	if IsUpstreamModelError(textRef("warning: nothing here"), config) {
		t.Fatal("custom substring matched unrelated text")
	}
	match := MatchUpstreamModelError(textRef("checkpoint-stall"), config)
	if !match.Matched || match.Pattern != "extra:CHECKPOINT-STALL" {
		t.Fatalf("unexpected match: %+v", match)
	}
}

func TestUpstreamErrorSupportsDelimitedCustomRegex(t *testing.T) {
	config := WakeFilterConfig{Enabled: true, ExtraPatterns: []string{"/foo\\s+bar/i"}}
	if !IsUpstreamModelError(textRef("FOO    bar"), config) {
		t.Fatal("delimited regex did not match")
	}
	if IsUpstreamModelError(textRef("foo-bar"), config) {
		t.Fatal("delimited regex matched a non-match")
	}
}

func TestUpstreamErrorCustomPatternsBypassShapedLatch(t *testing.T) {
	long := strings.Repeat("narrative ", 60) + "CHECKPOINT-STALL"
	if len(long) <= MaxErrorShapedChars {
		t.Fatal("text must exceed the error-shaped window")
	}
	if IsUpstreamModelError(textRef(long), DefaultWakeFilterConfig) {
		t.Fatal("default filter matched the long text")
	}
	config := WakeFilterConfig{Enabled: true, ExtraPatterns: []string{"checkpoint-stall"}}
	if !IsUpstreamModelError(textRef(long), config) {
		t.Fatal("custom pattern did not bypass the shaped latch")
	}
}

func TestUpstreamErrorSkipsInvalidCustomRegex(t *testing.T) {
	config := WakeFilterConfig{Enabled: true, ExtraPatterns: []string{"/[/"}}
	if !IsUpstreamModelError(textRef("API Error: 429 rate_limit_error"), config) {
		t.Fatal("built-in rule broke after an invalid custom pattern")
	}
	if IsUpstreamModelError(textRef("nothing to see"), config) {
		t.Fatal("invalid custom pattern matched everything")
	}
}

func TestUpstreamErrorNormalizesControlsAndWhitespace(t *testing.T) {
	if !isErrorShaped("\x1b[31mAPI Error: 429\x1b[0m") {
		t.Fatal("VT-wrapped error is not shaped")
	}
	if !IsUpstreamModelError(textRef("\x1b[31mAPI   Error:\n\t429\x1b[0m"), DefaultWakeFilterConfig) {
		t.Fatal("VT/whitespace error did not match")
	}
	if !IsUpstreamModelError(textRef("   read    ECONNRESET   "), DefaultWakeFilterConfig) {
		t.Fatal("padded error did not match")
	}
}

func TestUpstreamErrorIdempotentOnNormalizedInput(t *testing.T) {
	normalized := "API Error: 429 rate_limit_error"
	if !IsUpstreamModelError(textRef(normalized), DefaultWakeFilterConfig) {
		t.Fatal("normalized input did not match")
	}
	collapsed := strings.TrimSpace(whitespaceRunPattern.ReplaceAllString(normalized, " "))
	if !IsUpstreamModelError(textRef(collapsed), DefaultWakeFilterConfig) {
		t.Fatal("re-normalized input did not match")
	}
}

func TestUpstreamErrorPlainWordsAloneAreNotEnough(t *testing.T) {
	for _, text := range []string{"timeout", "error", "finished with an error in the log"} {
		if IsUpstreamModelError(textRef(text), DefaultWakeFilterConfig) {
			t.Fatalf("%q matched on its own", text)
		}
	}
}
