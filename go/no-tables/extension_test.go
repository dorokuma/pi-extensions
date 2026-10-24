package notables_test

import (
	"regexp"
	"strings"
	"testing"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
	notables "github.com/dorokuma/pi-extensions/go/no-tables"
)

// Verbatim shape of the assistant output that triggered the fence gluing bug.
const reportOutput = "**三、为什么 `none` 没加成**```\n" +
	"$ magpie model efforts workbuddy-ai/hy4-preview-f none,high\n" +
	"magpie: workbuddy-ai/hy4-preview-f has no reasoning level \"none\"   (exit 1)\n" +
	"```"

// ============================================================================
// Deployed + Repo TS Tests (1-12 common, 13-20 from repo copy)
// ============================================================================

func TestNormalizeMisplacedFences_GluedMovesToOwnLine(t *testing.T) {
	// TS Test: "a: a fence glued after text moves to its own line, code bytes unchanged"
	out := notables.NormalizeMisplacedFences(reportOutput)
	expected := strings.Replace(reportOutput, "**```", "**\n```", 1)
	if out != expected {
		t.Fatalf("expected %q, got %q", expected, out)
	}
	if !strings.Contains(out, "$ magpie model efforts workbuddy-ai/hy4-preview-f none,high") {
		t.Fatalf("code block body was corrupted: %s", out)
	}
}

func TestNormalizeMisplacedFences_AlreadyOnOwnLine(t *testing.T) {
	// TS Test: "b: a fence already on its own line is returned unchanged"
	ok := "**标题**\n```bash\necho hi\n```\n\n正常正文\n"
	if got := notables.NormalizeMisplacedFences(ok); got != ok {
		t.Fatalf("expected unchanged, got %q", got)
	}
}

func TestNormalizeMisplacedFences_TildeGlued(t *testing.T) {
	// TS Test: "c: tilde fences glued after text are handled the same way"
	glued := "正文~~~js\nlet a = 1\n~~~\n"
	want := "正文\n~~~js\nlet a = 1\n~~~\n"
	if got := notables.NormalizeMisplacedFences(glued); got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestNormalizeMisplacedFences_IndentedFencesMoveToColumn0(t *testing.T) {
	// TS Test: "d: indented fences move to column 0 without touching code lines"
	input := "前言\n  ```bash\n    echo hi\n    ```\n"
	want := "前言\n```bash\n    echo hi\n```\n"
	if got := notables.NormalizeMisplacedFences(input); got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestNormalizeMisplacedFences_InlineBackticksNeverTouched(t *testing.T) {
	// TS Test: "e: inline single and double backticks are never touched"
	cases := []string{
		"用 `code` 是行内代码\n",
		"用 ``code`` 也是行内代码\n",
		"文字``code``文字\n",
	}
	for _, s := range cases {
		if got := notables.NormalizeMisplacedFences(s); got != s {
			t.Fatalf("inline backticks were modified: input %q, got %q", s, got)
		}
	}
}

func TestNormalizeMisplacedFences_FenceMentionedMidSentence(t *testing.T) {
	// TS Test: "f: a fence only *mentioned* mid-sentence is not promoted to a real opener"
	mentions := []string{
		"明白，这是通用的渲染问题——模型把 ``` 贴在文字后面，切分就乱了。\n",
		"围栏 ``` 只有独占一行才算；一直到下一个 ``` 才闭合。\n",
		"put ``` at the start of a line instead\n",
		"a``` b```\n",
	}
	for _, s := range mentions {
		if got := notables.NormalizeMisplacedFences(s); got != s {
			t.Fatalf("must not fire on mention: input %q, got %q", s, got)
		}
	}
}

func TestNormalizeMisplacedFences_InsideBlockquote(t *testing.T) {
	// TS Test: "g: a fence inside a blockquote is already valid and is left alone"
	bq := "> 命令：\n> ```\n> echo hi\n> ```\n"
	if got := notables.NormalizeMisplacedFences(bq); got != bq {
		t.Fatalf("expected unchanged, got %q", got)
	}
}

func TestNormalizeMisplacedFences_Idempotent(t *testing.T) {
	// TS Test: "idempotent: normalizing twice equals normalizing once"
	inputs := []string{
		reportOutput,
		"正文~~~js\nlet a = 1\n~~~\n",
		"前言\n  ```bash\n    echo hi\n    ```\n",
		"**① 主效果**```text\nbody\n```\n",
		"文字```",
		"a```\nx\n```\nb```\ny\n```\n",
	}
	for _, s := range inputs {
		once := notables.NormalizeMisplacedFences(s)
		twice := notables.NormalizeMisplacedFences(once)
		if twice != once {
			t.Fatalf("not idempotent for input %q:\nonce:  %q\ntwice: %q", s, once, twice)
		}
	}
}

func TestNormalizeMisplacedFences_Edges(t *testing.T) {
	// TS Test: "edge: unclosed fence at end of input, consecutive fences, and a language tag"
	if got, want := notables.NormalizeMisplacedFences("文字```"), "文字\n```"; got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
	if got, want := notables.NormalizeMisplacedFences("a```\nx\n```\nb```\ny\n```\n"), "a\n```\nx\n```\nb\n```\ny\n```\n"; got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
	if got, want := notables.NormalizeMisplacedFences("文字```ts\nconst a = 1\n```\n"), "文字\n```ts\nconst a = 1\n```\n"; got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestNormalizeMisplacedFences_NestedInsideLongerFence(t *testing.T) {
	// TS Test: "edge: an indented fence that is code content inside a longer fence stays intact"
	nested := "说明：\n````markdown\n    ```\n    code\n    ```\n````\n"
	if got := notables.NormalizeMisplacedFences(nested); got != nested {
		t.Fatalf("nested fence inside longer fence modified: %q", got)
	}
}

func TestConvertUnsupportedMarkdown_HeadingFollowedByFenceNotReglued(t *testing.T) {
	// TS Test: "h: a heading immediately followed by a fence is not re-glued by rule A"
	cases := []string{
		"### 三、结论\n```\n$ cmd\n```\n\n后续正文\n",
		"### 三、结论\n\n```\n$ cmd\n```\n\n后续正文\n",
		"#### 小标题\n```bash\necho hi\n```\n尾部\n",
	}
	gluedPattern := regexp.MustCompile(`[^\n` + "`" + `~ \t>][ \t]*(` + "`" + `{3,}|~{3,})`)
	for _, md := range cases {
		out := notables.ConvertUnsupportedMarkdown(md)
		if gluedPattern.MatchString(out) {
			t.Fatalf("fence is still glued in output: %q", out)
		}
		if !strings.Contains(out, "**") {
			t.Fatalf("heading was not converted to bold: %q", out)
		}
	}
}

func TestConvertUnsupportedMarkdown_RulesAtoEApplyToProseAfterGluedFence(t *testing.T) {
	// TS Test: "rules A-E still apply to prose that a glued fence used to swallow"
	md := strings.Join([]string{
		"**三、为什么 `none` 没加成**```",
		"$ magpie ...   (exit 1)",
		"```",
		"",
		"### 四、结论",
		"",
		"正常正文。",
	}, "\n")
	out := notables.ConvertUnsupportedMarkdown(md)
	if !strings.HasPrefix(out, "**三、为什么 `none` 没加成**\n```\n") {
		t.Fatalf("unexpected prefix: %q", out)
	}
	if !strings.Contains(out, "**四、结论**") {
		t.Fatalf("heading rule A was not applied: %q", out)
	}
	if !strings.HasSuffix(out, "正常正文。") {
		t.Fatalf("unexpected suffix: %q", out)
	}
}

func TestRuleG_SpaceAfterClosingBoldWhenPrecededByPunctuation(t *testing.T) {
	// TS Test: "rule G: inserts a space after closing ** when preceded by punctuation and followed by text"
	input := "**实际用法举个例子：**女儿连续跑了"
	want := "**实际用法举个例子：** 女儿连续跑了"
	if got := notables.ConvertUnsupportedMarkdown(input); got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestRuleG_EndOfLineAndColonOutsideRemainUnchanged(t *testing.T) {
	// TS Test: "rule G: end-of-line closing ** and colon outside ** remain unchanged"
	cases := []string{
		"**它的用途是……：**",
		"**实际用法举个例子**：女儿连续跑了",
		"- **returned（回传量）**：行尾结束",
	}
	for _, s := range cases {
		if got := notables.ConvertUnsupportedMarkdown(s); got != s {
			t.Fatalf("expected unchanged for %q, got %q", s, got)
		}
	}
}

func TestRuleG_OpeningFollowedByTextAndClosingAfterNonPunctuation(t *testing.T) {
	// TS Test: "rule G: opening ** followed by text and closing ** after non-punctuation remain unchanged"
	cases := []string{
		"**x**是合法",
		"这是**加粗**测试",
	}
	for _, s := range cases {
		if got := notables.ConvertUnsupportedMarkdown(s); got != s {
			t.Fatalf("expected unchanged for %q, got %q", s, got)
		}
	}
}

func TestRuleG_Idempotent(t *testing.T) {
	// TS Test: "rule G: idempotent when run again on already-repaired text"
	input := "**实际用法举个例子：**女儿连续跑了"
	once := notables.ConvertUnsupportedMarkdown(input)
	want := "**实际用法举个例子：** 女儿连续跑了"
	if once != want {
		t.Fatalf("expected %q, got %q", want, once)
	}
	if twice := notables.ConvertUnsupportedMarkdown(once); twice != once {
		t.Fatalf("not idempotent: %q vs %q", once, twice)
	}
}

func TestRuleG_SingleAsteriskEmphasisUntouched(t *testing.T) {
	// TS Test: "rule G: single-asterisk emphasis is out of scope and left untouched"
	cases := []string{
		"*text*：后文",
		"*text：*后文",
	}
	for _, s := range cases {
		if got := notables.ConvertUnsupportedMarkdown(s); got != s {
			t.Fatalf("single asterisk emphasis should not be changed: %q, got %q", s, got)
		}
	}
}

func TestRuleG_InlineCodeAndAlignedClosingPunctuationUntouched(t *testing.T) {
	// TS Test: "rule G: inline backtick code spans and aligned closing punctuation are left untouched"
	cases := []string{
		"`**实际用法：**例子`",
		"``**实际用法：**例子``",
		"**a：**～x",
		"**a：**\"引用\"",
	}
	for _, s := range cases {
		if got := notables.ConvertUnsupportedMarkdown(s); got != s {
			t.Fatalf("expected unchanged for %q, got %q", s, got)
		}
	}
	mixed := "行内 `**a：**b` 不变，但**实际用法：**例子要补空格"
	want := "行内 `**a：**b` 不变，但**实际用法：** 例子要补空格"
	if got := notables.ConvertUnsupportedMarkdown(mixed); got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestRuleG_BoldSpanContainingInlineCode(t *testing.T) {
	// TS Test: "rule G: bold span containing inline code inserts space after closing ** and is idempotent"
	input := "**`--flag`：**强制执行"
	once := notables.ConvertUnsupportedMarkdown(input)
	want := "**`--flag`：** 强制执行"
	if once != want {
		t.Fatalf("expected %q, got %q", want, once)
	}
	if !strings.Contains(once, "`--flag`") {
		t.Fatalf("lost code span: %q", once)
	}
	if twice := notables.ConvertUnsupportedMarkdown(once); twice != once {
		t.Fatalf("not idempotent: %q vs %q", once, twice)
	}
}

func TestRuleE_LinkLabelContainingInlineCode(t *testing.T) {
	// TS Test: "rule E: link label containing inline code is flattened without altering code span and is idempotent"
	input := "see [`docs`](https://example.com) and [`**a：**b`](https://example.com)"
	once := notables.ConvertUnsupportedMarkdown(input)
	want := "see `docs` https://example.com and `**a：**b` https://example.com"
	if once != want {
		t.Fatalf("expected %q, got %q", want, once)
	}
	if !strings.Contains(once, "`docs`") || !strings.Contains(once, "`**a：**b`") {
		t.Fatalf("code spans corrupted: %q", once)
	}
	if twice := notables.ConvertUnsupportedMarkdown(once); twice != once {
		t.Fatalf("not idempotent: %q vs %q", once, twice)
	}
}

// ============================================================================
// Table conversion tests (ConvertTables)
// ============================================================================

func TestConvertTables_Basic(t *testing.T) {
	table := "| 字段 | 类型 | 说明 |\n| --- | --- | --- |\n| id | int | 主键 |\n| name | string | 名称 |\n"
	want := "- **id** — 类型: int, 说明: 主键\n- **name** — 类型: string, 说明: 名称\n"
	got := notables.ConvertTables(table)
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestConvertTables_SingleColumn(t *testing.T) {
	table := "| 选项 |\n| --- |\n| Option A |\n| Option B |\n"
	want := "- Option A\n- Option B\n"
	got := notables.ConvertTables(table)
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestConvertTables_EmptyHeaderColumn(t *testing.T) {
	table := "| 键 |  |\n| --- | --- |\n| foo | bar |\n"
	want := "- **foo** — bar\n"
	got := notables.ConvertTables(table)
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestConvertTables_MultipleTables(t *testing.T) {
	input := "前面正文\n\n" +
		"| A | B |\n| --- | --- |\n| 1 | 2 |\n\n" +
		"中间正文\n\n" +
		"| X | Y |\n| --- | --- |\n| 8 | 9 |\n\n" +
		"结尾"
	got := notables.ConvertTables(input)
	if !strings.Contains(got, "- **1** — B: 2") {
		t.Fatalf("table 1 not converted: %s", got)
	}
	if !strings.Contains(got, "- **8** — Y: 9") {
		t.Fatalf("table 2 not converted: %s", got)
	}
	if !strings.Contains(got, "中间正文") {
		t.Fatalf("prose corrupted: %s", got)
	}
}

// ============================================================================
// Additional markdown rules (Rule A, B, C, D, E) & Boundaries
// ============================================================================

func TestConvertUnsupportedMarkdown_ImagesAndFootnotes(t *testing.T) {
	// Rule B: image with alt text -> [label](url)
	img1 := "![logo](https://example.com/logo.png)"
	if got := notables.ConvertUnsupportedMarkdown(img1); got != "logo https://example.com/logo.png" {
		// Notice Rule B turns ![logo](url) into [logo](url), and Rule E then turns [logo](url) into "logo url"
		t.Fatalf("image conversion failed: %q", got)
	}
	// Rule B: image with empty alt -> [image](url) -> "image url"
	imgEmpty := "![](https://example.com/a.png)"
	if got := notables.ConvertUnsupportedMarkdown(imgEmpty); got != "image https://example.com/a.png" {
		t.Fatalf("empty alt image conversion failed: %q", got)
	}

	// Rule C & D: footnotes
	fn := "正文[^note1]引用。\n\n[^note1]: 脚注详细内容"
	got := notables.ConvertUnsupportedMarkdown(fn)
	if !strings.Contains(got, "正文[note1]引用。") {
		t.Fatalf("footnote reference not converted: %q", got)
	}
	if !strings.Contains(got, "**[note1]** 脚注详细内容") {
		t.Fatalf("footnote definition not converted: %q", got)
	}
}

func TestConvertUnsupportedMarkdown_Headings(t *testing.T) {
	h := "### 三级标题\n#### 四级标题\n##### 五级标题\n###### 六级标题\n"
	got := notables.ConvertUnsupportedMarkdown(h)
	want := "**三级标题**\n**四级标题**\n**五级标题**\n**六级标题**"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestConvertUnsupportedMarkdown_Links(t *testing.T) {
	// Same label and url
	l1 := "[https://example.com](https://example.com)"
	if got := notables.ConvertUnsupportedMarkdown(l1); got != "https://example.com" {
		t.Fatalf("identical label/url link failed: %q", got)
	}
	// Empty label
	l2 := "[](https://example.com)"
	if got := notables.ConvertUnsupportedMarkdown(l2); got != "https://example.com" {
		t.Fatalf("empty label link failed: %q", got)
	}
}

func TestConvertUnsupportedMarkdown_Boundaries(t *testing.T) {
	// 1. Empty string
	if got := notables.ConvertUnsupportedMarkdown(""); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
	if got := notables.ConvertTables(""); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}

	// 2. Pure code block
	pureCode := "```go\nfunc main() {\n\tprintln(\"hello\")\n}\n```"
	if got := notables.ConvertUnsupportedMarkdown(pureCode); got != pureCode {
		t.Fatalf("pure code block modified: %q", got)
	}

	// 3. Already plain text
	plain := "这是一段没有任何 Markdown 格式的普通中文纯文本。"
	if got := notables.ConvertUnsupportedMarkdown(plain); got != plain {
		t.Fatalf("plain text modified: %q", got)
	}

	// 4. CRLF line endings (preserved verbatim, matching TS)
	crlfTable := "| 名字 | 分数 |\r\n| --- | --- |\r\n| Alice | 100 |\r\n"
	if got := notables.ConvertTables(crlfTable); got != crlfTable {
		t.Fatalf("expected CRLF table untouched, got %q", got)
	}
}

// ============================================================================
// Event Hook & Extension Integration Tests
// ============================================================================

func TestHandleMessageEnd_NonAssistant(t *testing.T) {
	userPayload := map[string]any{
		"message": map[string]any{
			"role":    "user",
			"content": "| A | B |\n| --- | --- |\n| 1 | 2 |\n",
		},
	}
	res, err := notables.HandleMessageEnd(sdk.Context{}, userPayload)
	if err != nil || res != nil {
		t.Fatalf("user message should not be modified, got res=%v, err=%v", res, err)
	}
}

func TestHandleMessageEnd_AssistantNoChange(t *testing.T) {
	asstPayload := map[string]any{
		"message": map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type": "text",
					"text": "这是一段普通的回答，无需修改。",
				},
			},
		},
	}
	res, err := notables.HandleMessageEnd(sdk.Context{}, asstPayload)
	if err != nil || res != nil {
		t.Fatalf("assistant message without tables should return nil, got %v", res)
	}
}

func TestHandleMessageEnd_AssistantWithTable(t *testing.T) {
	originalText := "| 计划 | 状态 |\n| --- | --- |\n| 任务1 | 完成 |\n"
	asstPayload := map[string]any{
		"message": map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type": "text",
					"text": originalText,
				},
				map[string]any{
					"type": "thinking",
					"text": "思考中...",
				},
			},
		},
	}

	res, err := notables.HandleMessageEnd(sdk.Context{}, asstPayload)
	if err != nil {
		t.Fatalf("HandleMessageEnd error: %v", err)
	}
	if res == nil {
		t.Fatalf("expected rewritten message, got nil")
	}

	resMap, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", res)
	}
	msg, ok := resMap["message"].(map[string]any)
	if !ok {
		t.Fatalf("expected message map, got %v", resMap)
	}

	content, ok := msg["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("unexpected content: %v", msg["content"])
	}

	part0 := content[0].(map[string]any)
	text0 := part0["text"].(string)
	if !strings.Contains(text0, "- **任务1** — 状态: 完成") {
		t.Fatalf("table was not converted in text part: %s", text0)
	}

	// Thinking part is unchanged
	part1 := content[1].(map[string]any)
	if part1["type"] != "thinking" || part1["text"] != "思考中..." {
		t.Fatalf("non-text part corrupted: %v", part1)
	}

	// Original payload remains unmutated
	origPart0 := asstPayload["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if origPart0["text"] != originalText {
		t.Fatalf("original payload was mutated in-place!")
	}
}

func TestExtension_Construct(t *testing.T) {
	ext := notables.Extension()
	if ext == nil {
		t.Fatal("Extension() returned nil")
	}
	if ext.Name() != "no-tables" {
		t.Fatalf("expected name 'no-tables', got %q", ext.Name())
	}
}

func TestJSWhitespace_HeadingsAndFootnotes(t *testing.T) {
	// F1 counterexamples: JS \s includes Unicode spaces like \u3000, \u00a0, etc.
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// 4 counterexamples
		{"heading_ideographic_space", "###\u3000标题", "**标题**"},
		{"heading_nbsp", "###\u00a0标题", "**标题**"},
		{"heading_trailing_ideographic", "### a\u3000", "**a**"},
		{"footdef_ideographic_space", "[^1]:\u3000text", "**[1]** text"},
		// 2 combinations
		{"combo_heading_mixed_spaces", "###\u3000\u00a0标题\u2003", "**标题**"},
		{"combo_footdef_mixed_spaces", "[^2]:\u00a0\u3000combined text", "**[2]** combined text"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := notables.ConvertUnsupportedMarkdown(tt.input)
			if got != tt.want {
				t.Fatalf("ConvertUnsupportedMarkdown(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestCRLF_VerbatimPreserved(t *testing.T) {
	// CRLF inputs get no extra leniency: these cases stay byte-identical to the TS
	// authority (a CRLF table never matches, and a CRLF glued fence is not promoted).
	t.Run("crlf_table_preserved", func(t *testing.T) {
		table := "| 名字 | 分数 |\r\n| --- | --- |\r\n| Alice | 100 |\r\n"
		got := notables.ConvertTables(table)
		if got != table {
			t.Fatalf("expected CRLF table untouched, got %q", got)
		}
	})

	t.Run("crlf_glued_fence_preserved", func(t *testing.T) {
		fence := "正文```\r\ncode\r\n```\r\n"
		got := notables.NormalizeMisplacedFences(fence)
		if got != fence {
			t.Fatalf("expected CRLF glued fence untouched, got %q", got)
		}
	})

	t.Run("crlf_link_preserved", func(t *testing.T) {
		link := "[label](https://example.com\r\n)"
		got := notables.ConvertUnsupportedMarkdown(link)
		if got != link {
			t.Fatalf("expected CRLF link untouched, got %q", got)
		}
	})
}

func TestDocumentedDeviations_U2028(t *testing.T) {
	// F4 & Observation 8: Go RE2 does not recognize U+2028 as line break in (?m),
	// so "### 标题\u2028正文" is parsed as a single heading span containing U+2028.
	input := "### 标题\u2028正文"
	got := notables.ConvertUnsupportedMarkdown(input)
	want := "**标题\u2028正文**"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

// ============================================================================
// CRLF / CR line-terminator parity with the TS authority
// ============================================================================
//
// 所有期望值均由直接运行 TS 权威源（extensions/no-tables/src/no-tables.ts，node）
// 产出，不是手写推导；对比方法见 README「CRLF / CR 行终止符对齐」。

func pipeline(s string) string {
	return notables.ConvertUnsupportedMarkdown(notables.ConvertTables(s))
}

func TestLineTerminatorParity_READMEItems(t *testing.T) {
	// 原「下一批待修」清单条目 1–5、7（条目 6、8 见 U+2028/U+2029 文档化分歧）。
	tests := []struct {
		name  string
		input string
		want  string // TS 输出
	}{
		{
			name:  "1_crlf_indented_empty_fence",
			input: "```\r\n    ```\r\n",
			want:  "```\r\n```\r\n",
		},
		{
			name:  "2_crlf_indented_fence_with_info",
			input: "```bash\r\n    ```\r\n",
			want:  "```bash\r\n```\r\n",
		},
		{
			name:  "3_crlf_code_indented_closer",
			input: "```\r\ncode\r\n    ```\r\n",
			want:  "```\r\ncode\r\n```\r\n",
		},
		{
			name:  "4_crlf_nested_indented_fence",
			input: "````markdown\r\n    ```\r\n    code\r\n    ```\r\n````\r\n",
			want:  "````markdown\r\n```\r\n    code\r\n```\r\n````\r\n",
		},
		{
			name:  "5_heading_cr",
			input: "### a\rb",
			want:  "**a**\rb",
		},
		{
			name:  "7_table_with_cr",
			input: "| a\rb | c |\n| - | - |\n| 1 | 2 |\n",
			want:  "| a\rb | c |\n| - | - |\n| 1 | 2 |\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pipeline(tt.input); got != tt.want {
				t.Fatalf("pipeline(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestLineTerminatorParity_RuleCoverage(t *testing.T) {
	// 逐规则覆盖 \n / \r\n / \r：Rule A（标题）、B（图片）、C/D（脚注）、E（链接）、
	// F（围栏）、G（粗体闭合）与表格转换。
	tests := []struct {
		name  string
		input string
		want  string // TS 输出
	}{
		// Rule A: JS /m 的 ^/$ 在裸 CR 处也断行，且 . 不跨 CR。
		{"heading_cr", "### a\rb", "**a**\rb"},
		{"heading_crlf", "### a\r\nb", "**a**\nb"},
		{"heading_cr_after_hash_run", "###\r标题", "**标题**"},
		{"heading_double_cr_then_next_heading", "### a\r\r#### b", "**a**\r**b**"},
		{"heading_after_bare_cr_line_start", "a\r\r### b", "a\r\r**b**"},
		{"heading_mixed_terminators", "### a\n\r### b\r\n#### c\r", "**a**\r**b**\n**c**"},
		{"heading_whitespace_crossing_terminators", "### a\u3000\r\n\r\n###\ta\r\r#### 四级", "**a**\n**a**\r**四级**"},
		{"heading_crlf_prose", "### 标题\r\n正文", "**标题**\n正文"},
		// Rule D: 脚注定义行（\s* 可自行吃掉 CRLF）。
		{"footdef_crlf_trailing", "[^1]: x\r\n", "**[1]** x\r\n"},
		{"footdef_ws_crossing_crlf", "[^1]:\r\nbody", "**[1]** body"},
		{"footdef_bare_cr", "[^1]:x\rb", "**[1]** x\rb"},
		// 表格：JS 的 . 不跨 CR，含裸 CR 的单元格保持原样（不转 bullet）。
		{"table_cr_cell_untouched", "| a\rb | c |\n| - | - |\n| 1 | 2 |\n", "| a\rb | c |\n| - | - |\n| 1 | 2 |\n"},
		{"table_crlf_untouched", "| a | b |\r\n| - | - |\r\n| 1 | 2 |\r\n", "| a | b |\r\n| - | - |\r\n| 1 | 2 |\r\n"},
		{"table_row_greedy_partial", "| a | b |\n| - | - |\n| 1 | 2\r |\n", "- 1\n 2\r |\n"},
		// Rule F：CRLF/CR 行尾使 TS 的围栏状态机不触发，只有 F2 去缩进生效。
		{"fence_glued_crlf_untouched", "正文```\r\ncode\r\n```\r\n", "正文```\r\ncode\r\n```\r\n"},
		{"fence_glued_cr_untouched", "正文```\rcode\r```\r", "正文```\rcode\r```\r"},
		// Rule B / E / C：行内规则在 CRLF/CR 上下文中照常生效。
		{"image_crlf_trailing", "![logo](https://x.com/a.png)\r\n", "logo https://x.com/a.png\r\n"},
		{"link_crlf_trailing", "[label](https://x.com)\r\n", "label https://x.com\r\n"},
		{"footref_crlf", "正文[^n1]引用\r\n", "正文[n1]引用\r\n"},
		{"image_crlf_broken_span_untouched", "![alt](https://x.com\r\n)", "![alt](https://x.com\r\n)"},
		// Rule G：闭合 ** 后的换行不能被当作需要补空格的后续文本。
		{"bold_crlf_untouched", "**实际用法举个例子：**\r\n女儿", "**实际用法举个例子：**\r\n女儿"},
		{"bold_cr_untouched", "**实际用法举个例子：**\r女儿", "**实际用法举个例子：**\r女儿"},
		{"inline_code_and_bold_crlf", "行内 `**a：**b` 不变，但**实际用法：**例子要补空格\r\n", "行内 `**a：**b` 不变，但**实际用法：** 例子要补空格\r\n"},
		// 混合行终止符 + 全流程。
		{"inline_code_before_crlf_heading", "`code`\r\n### t\r\n", "`code`\r\n**t**"},
		{"mixed_document", "### 标题\r\n| a | b |\n| - | - |\n| 1 | 2 |\n\r\n[^x]: 定义\r\n", "**标题**\n- **1** — b: 2\n\r\n**[x]** 定义\r\n"},
		{"crlf_report_output_untouched", "**三、为什么 `none` 没加成**```\r\n$ magpie\r\n```\r\n", "**三、为什么 `none` 没加成**```\r\n$ magpie\r\n```\r\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pipeline(tt.input); got != tt.want {
				t.Fatalf("pipeline(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestDocumentedDeviations_U2028U2029(t *testing.T) {
	// 仍需文档化、不计划修的 U+2028 / U+2029 分歧（README 条目 6、8）。
	tests := []struct {
		name  string
		input string
		want  string // 当前 Go 输出（与 TS 不一致）
	}{
		{"6_heading_u2029", "### a\u2029b", "**a\u2029b**"},
		{"8a_link_with_u2028", "[a\u2028b](https://x.com)", "a\u2028b https://x.com"},
		{"8b_image_with_u2028", "![a\u2028b](https://x.com/a.png)", "a\u2028b https://x.com/a.png"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pipeline(tt.input); got != tt.want {
				t.Fatalf("pipeline(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
