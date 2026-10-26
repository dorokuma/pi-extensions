package pivcc

import (
	"strings"
	"testing"
)

// Tests for src/extract/* and src/core/build-sections.ts: the extractors that
// turn a normalized transcript into SectionData.

// ── user messages ────────────────────────────────────────────────────────

// TestExtractUserMessages_FirstMessagesAreTheEarlyRequests pins
// user-messages.ts extractUserMessages(): what the user wrote, oldest first,
// verbatim and clipped.
func TestExtractUserMessages_FirstMessagesAreTheEarlyRequests(t *testing.T) {
	blocks := []NormalizedBlock{
		userBlock("port the extractor to Go", 0, true),
		userBlock("ok", 1, true),             // one word: dropped
		userBlock("好的", 2, true),             // one word: dropped
		userBlock("keep the patch", 3, true), // kept
	}
	got := ExtractUserMessages(blocks)
	want := []string{"port the extractor to Go", "keep the patch"}
	if len(got) != len(want) {
		t.Fatalf("ExtractUserMessages = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ExtractUserMessages = %#v, want %#v", got, want)
		}
	}
}

// TestExtractUserMessages_ChineseMessagesAreKept pins the word-count floor in
// the language this extension is used in most: a Chinese instruction is not a
// one-word reply.
func TestExtractUserMessages_ChineseMessagesAreKept(t *testing.T) {
	blocks := []NormalizedBlock{
		userBlock("把 Go 版 pi-vcc 同步升级到上游 0.11.1，并保留两个本地补丁", 0, true),
		userBlock("这个报错是什么原因，帮我看看日志", 1, true),
	}
	got := ExtractUserMessages(blocks)
	if len(got) != 2 {
		t.Fatalf("ExtractUserMessages = %#v, want both messages", got)
	}
}

// TestExtractUserMessages_InjectedIsLeftOut pins the 0.11.0 rule: a message an
// extension injected (a custom_message) is not the user's words.
func TestExtractUserMessages_InjectedIsLeftOut(t *testing.T) {
	blocks := []NormalizedBlock{
		userBlock("the real task", 0, true),
		userBlockInjected("an extension note", 1, true, true),
		userBlock("a follow-up question", 2, true),
	}
	got := ExtractUserMessages(blocks)
	if len(got) != 2 || got[0] != "the real task" || got[1] != "a follow-up question" {
		t.Fatalf("ExtractUserMessages = %#v, want the two user messages", got)
	}
}

// TestExtractUserMessages_DropsToolOpenedText pins userLine()'s opening-tag
// rule: text that opens with a tag was put there by a tool.
func TestExtractUserMessages_DropsToolOpenedText(t *testing.T) {
	blocks := []NormalizedBlock{
		userBlock("<peer_message>from a colleague</peer_message>", 0, true),
		userBlock("<local-command-stdout>done</local-command-stdout>", 1, true),
		userBlock("a real request", 2, true),
	}
	got := ExtractUserMessages(blocks)
	if len(got) != 1 || got[0] != "a real request" {
		t.Fatalf("ExtractUserMessages = %#v, want only the real request", got)
	}
}

// TestExtractUserMessages_SkillTextKeepsTheTag pins userLine()'s skill rule: an
// expanded skill alone is not the user's words, so its text is kept behind a
// `[skill: name]` tag.
func TestExtractUserMessages_SkillTextKeepsTheTag(t *testing.T) {
	blocks := []NormalizedBlock{
		userBlock("<skill name=\"pdf\">a long card body\nwith two lines</skill>\nplease read the PDF report", 0, true),
	}
	got := ExtractUserMessages(blocks)
	if len(got) != 1 {
		t.Fatalf("ExtractUserMessages = %#v", got)
	}
	if !strings.HasPrefix(got[0], "[skill: pdf] ") {
		t.Fatalf("the skill tag was lost: %q", got[0])
	}
	if !strings.Contains(got[0], "please read the PDF report") {
		t.Fatalf("the user's text was lost: %q", got[0])
	}
}

// TestKeepNewestUserMessages pins keepNewestUserMessages(): the newest 15,
// oldest first, a repeat moving to the end.
func TestKeepNewestUserMessages(t *testing.T) {
	lines := make([]string, 0, 17)
	for i := 0; i < 17; i++ {
		lines = append(lines, "line "+jsNumberString(float64(i)))
	}
	lines = append(lines, "line 0") // a repeat moves to the end
	got := KeepNewestUserMessages(lines)
	if len(got) != 15 {
		t.Fatalf("KeepNewestUserMessages kept %d lines, want 15: %#v", len(got), got)
	}
	if got[14] != "line 0" || got[0] != "line 3" {
		t.Fatalf("KeepNewestUserMessages = %#v", got)
	}
}

// TestUserLine_ClipsAt300 pins USER_MESSAGE_CHARS.
func TestUserLine_ClipsAt300(t *testing.T) {
	long := "run the full verification suite for the whole repository " + strings.Repeat("and again ", 40)
	got := userLine(long)
	if jsLen(got) > 300 {
		t.Fatalf("userLine kept %d units, over the 300-char clip", jsLen(got))
	}
}

// TestUserMessageWordCount_IntlSegmenterCalibration pins the Go word count
// against TS's `new Intl.Segmenter(undefined, { granularity: "word" })` on this
// host, where the default locale resolves to zh-CN and ICU breaks CJK with its
// Chinese dictionary. The `want` column was measured with node's Intl.Segmenter
// (the reference implementation); `goWant` is what this port counts, and is what
// the test pins. One long sentence is the only row where they differ: ICU's
// dictionary splits one Han run into one word more than the ceil(n/2)
// approximation in userMessages.go, which never changes an outcome because the
// count only decides whether a message has at least two words.
func TestUserMessageWordCount_IntlSegmenterCalibration(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   int // Intl.Segmenter (zh-CN)
		goWant int // userMessageWordCount
	}{
		{"an English sentence", "hello world", 2, 2},
		{"a contraction is one word", "don't", 1, 1},
		{"a hyphenated pair is two", "e-mail", 2, 2},
		{"a dotted number is one", "3.14", 1, 1},
		{"a dashed number is two", "3-5", 2, 2},
		{"korean joins per run", "한글 테스트", 2, 2},
		{"chinese is dictionary broken", "中文消息测试", 3, 3},
		{"a two-character reply is one word", "好的", 1, 1},
		{"a three-character sentence is two", "没问题", 2, 2},
		{"kana counts per character", "ひらがな", 4, 4},
		{"mixed chinese and latin", "中文a测试", 3, 3},
		{"punctuation only", "（）【】", 0, 0},
		{"emoji is not a word", "🎉 很好", 1, 1},
		{"a real instruction", "把 Go 版 pi-vcc 同步升级到上游 0.11.1 并且保留两个本地补丁", 16, 15},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := userMessageWordCount(test.in); got != test.goWant {
				t.Fatalf("userMessageWordCount(%q) = %d, want %d (Intl.Segmenter counted %d)",
					test.in, got, test.goWant, test.want)
			}
		})
	}
}

// TestUserMessageWordCount_ShortRepliesAreDocumented pins the short replies
// against the ICU measurement: "好的" is one ICU word and one word here, while
// ICU's dictionary splits the other two-character replies into two. Those are
// read as one word here, so a two-character reply can be dropped where TS would
// keep it. Nothing substantive is ever dropped: every longer sentence agrees
// with ICU (see the calibration table above), and the outcome that matters —
// a real instruction is never dropped — is pinned by
// TestExtractUserMessages_ChineseMessagesAreKept.
func TestUserMessageWordCount_ShortRepliesAreDocumented(t *testing.T) {
	tests := []struct {
		text string
		icu  int
		go_  int
	}{
		{"好的", 1, 1},
		{"走吧", 2, 1},
		{"是的", 2, 1},
		{"没事", 2, 1},
	}
	for _, test := range tests {
		if got := userMessageWordCount(test.text); got != test.go_ {
			t.Fatalf("userMessageWordCount(%q) = %d, want %d (Intl.Segmenter counted %d)",
				test.text, got, test.go_, test.icu)
		}
	}
}

// ── files ───────────────────────────────────────────────────────────────────

func TestExtractFiles(t *testing.T) {
	blocks := []NormalizedBlock{
		{Kind: KindToolCall, Name: "read", Args: map[string]any{"path": "/root/p/a.go"}},
		{Kind: KindToolCall, Name: "edit", Args: map[string]any{"path": "/root/p/b.go"}},
		{Kind: KindToolCall, Name: "write", Args: map[string]any{"path": "/root/p/c.go"}},
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "ls"}},
		// Case-insensitive tool matching.
		{Kind: KindToolCall, Name: "Read", Args: map[string]any{"file_path": "/root/p/d.go"}},
	}
	activity := ExtractFiles(blocks, nil)
	if !activity.Read.Has("a.go") || !activity.Read.Has("d.go") {
		t.Fatalf("Read = %v", activity.Read.Items())
	}
	if !activity.Modified.Has("b.go") {
		t.Fatalf("Modified = %v", activity.Modified.Items())
	}
	// write is both a write and a create tool.
	if !activity.Created.Has("c.go") || !activity.Modified.Has("c.go") {
		t.Fatalf("Created = %v Modified = %v", activity.Created.Items(), activity.Modified.Items())
	}
}

func TestExtractFiles_MergesFileOps(t *testing.T) {
	activity := ExtractFiles(nil, &FileOps{
		ReadFiles:     []string{"/root/p/x.go"},
		ModifiedFiles: []string{"/root/p/y.go"},
		CreatedFiles:  []string{"/root/p/z.go"},
	})
	if !activity.Modified.Has("y.go") || !activity.Created.Has("z.go") || !activity.Read.Has("x.go") {
		t.Fatalf("hook FileOps were not merged: %#v", activity)
	}
}

func TestLongestCommonDirPrefix(t *testing.T) {
	tests := []struct {
		name  string
		paths []string
		want  string
	}{
		{"two under a deep root", []string{"/root/p/q/a.go", "/root/p/q/b.go"}, "/root/p/q/"},
		// split("/a/x.go") = ["", "a", "x.go"]; the loop reaches i=2, which
		// clears the `i < 2` guard, so /a/ is a valid trim prefix.
		{"root plus one segment", []string{"/a/x.go", "/a/y.go"}, "/a/"},
		{"no common prefix", []string{"/a/x.go", "/b/y.go"}, ""},
		{"one path only", []string{"/root/p/a.go"}, ""},
		{"relative paths ignored", []string{"a.go", "b.go"}, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := longestCommonDirPrefix(test.paths); got != test.want {
				t.Fatalf("longestCommonDirPrefix(%v) = %q, want %q", test.paths, got, test.want)
			}
		})
	}
}

func TestFormatFileActivity(t *testing.T) {
	blocks := []NormalizedBlock{
		{Kind: KindToolCall, Name: "edit", Args: map[string]any{"path": "/root/p/a.go"}},
		{Kind: KindToolCall, Name: "write", Args: map[string]any{"path": "/root/p/b.go"}},
		{Kind: KindToolCall, Name: "read", Args: map[string]any{"path": "/root/p/c.go"}},
	}
	lines := formatFileActivity(blocks, nil)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "Modified: a.go, b.go") {
		t.Fatalf("Modified line wrong:\n%s", joined)
	}
	// b.go was written, so it must not also appear under Created.
	if strings.Contains(joined, "Created:") {
		t.Fatalf("a modified file also appears as Created:\n%s", joined)
	}
	if !strings.Contains(joined, "Read: c.go") {
		t.Fatalf("Read line missing:\n%s", joined)
	}
}

func TestFormatFileActivity_CapsCategoryAtTen(t *testing.T) {
	blocks := make([]NormalizedBlock, 0, 12)
	for i := 0; i < 12; i++ {
		name := "f" + jsNumberString(float64(i)) + ".go"
		blocks = append(blocks, NormalizedBlock{
			Kind: KindToolCall, Name: "read",
			Args: map[string]any{"path": "/root/p/" + name},
		})
	}
	joined := strings.Join(formatFileActivity(blocks, nil), "\n")
	if !strings.Contains(joined, "(+2 earlier) f2.go, f3.go, f4.go, f5.go, f6.go, f7.go, f8.go, f9.go, f10.go, f11.go") {
		t.Fatalf("the cap kept the wrong files:\n%s", joined)
	}
	if strings.Contains(joined, "f0.go") || strings.Contains(joined, "f1.go") {
		t.Fatalf("the oldest files survived the cap:\n%s", joined)
	}
}

// ── commits ─────────────────────────────────────────────────────────────────

func TestExtractCommits_FromCommandMessage(t *testing.T) {
	// extractCommits reads tool_call bash blocks and pairs the hash from the
	// following tool_result, exactly as commits.ts does.
	blocks := []NormalizedBlock{
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": `git commit -m "fix: handle expiry"`}},
		{Kind: KindToolResult, Name: "bash", Text: "[main abc1234] fix: handle expiry"},
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "git status"}},
	}
	got := ExtractCommits(blocks)
	if len(got) != 1 {
		t.Fatalf("ExtractCommits = %#v, want one commit", got)
	}
	if got[0].Message != "fix: handle expiry" {
		t.Fatalf("commit message = %q", got[0].Message)
	}
	if !got[0].HasHash || got[0].Hash != "abc1234" {
		t.Fatalf("hash not paired from the tool_result: %#v", got[0])
	}
}

func TestExtractCommits_AmendWithQuotedMessage(t *testing.T) {
	// An amend with a fresh quoted message records the new sha through the same
	// COMMIT_MSG_RE path (the regex requires a quoted -m subject).
	got := ExtractCommits([]NormalizedBlock{
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": `git commit --amend -m "fix: reworded"`}},
		{Kind: KindToolResult, Name: "bash", Text: "[main cafe123] fix: reworded"},
	})
	if len(got) != 1 || got[0].Hash != "cafe123" || got[0].Message != "fix: reworded" {
		t.Fatalf("ExtractCommits(amend) = %#v", got)
	}
}

func TestFormatCommits(t *testing.T) {
	commits := []CommitInfo{
		{Hash: "aaaaaaa", HasHash: true, Message: "first"},
		{Message: "no hash"},
		{Hash: "bbbbbbb", HasHash: true, Message: "third"},
	}
	got := FormatCommits(commits, 8)
	want := []string{"aaaaaaa: first", "no hash", "bbbbbbb: third"}
	if len(got) != len(want) {
		t.Fatalf("FormatCommits = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("FormatCommits = %v, want %v", got, want)
		}
	}
	// The cap keeps the most recent.
	if got := FormatCommits(commits, 2); len(got) != 2 || got[0] != "no hash" {
		t.Fatalf("FormatCommits cap kept the wrong end: %v", got)
	}
}

// ── tracked commands ────────────────────────────────────────────────────────

func TestExtractTrackedCommands(t *testing.T) {
	blocks := []NormalizedBlock{
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "go test ./..."}},
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "go build ./cmd/x && go vet ./..."}},
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "ls -la"}},
	}
	activity := ExtractTrackedCommands(blocks, []string{"go", "bun"})
	got := FormatTrackedCommands(activity)
	if len(got) != 1 || !strings.HasPrefix(got[0], "go: ") {
		t.Fatalf("FormatTrackedCommands = %v", got)
	}
	joined := got[0]
	for _, want := range []string{"go test ./...", "go build ./cmd/x", "go vet ./..."} {
		if !strings.Contains(joined, want) {
			t.Fatalf("a tracked invocation was lost:\n%s", joined)
		}
	}
}

func TestExtractTrackedCommands_CtxRunPatchShapes(t *testing.T) {
	blocks := []NormalizedBlock{
		// argv array form.
		{Kind: KindToolCall, Name: "ctx_run", Args: map[string]any{
			"argv": []any{"go", "test", "./..."},
		}},
		// batch commands array form.
		{Kind: KindToolCall, Name: "ctx_run", Args: map[string]any{
			"commands": []any{"go vet ./...", map[string]any{"command": "go build ./..."}},
		}},
	}
	activity := ExtractTrackedCommands(blocks, []string{"go"})
	entries := activity.ByCommand["go"]
	joined := strings.Join(entries, " | ")
	for _, want := range []string{"go test ./...", "go vet ./...", "go build ./..."} {
		if !strings.Contains(joined, want) {
			t.Fatalf("a patch command shape was not tracked:\n%s", joined)
		}
	}
}

// TestExtractTrackedCommands_ReRunMovesToTail lives in patchCtxRun_test.go.

func TestExtractTrackedCommands_EmptyConfig(t *testing.T) {
	blocks := []NormalizedBlock{
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "go test ./..."}},
	}
	activity := ExtractTrackedCommands(blocks, nil)
	if len(activity.Order) != 0 || len(FormatTrackedCommands(activity)) != 0 {
		t.Fatalf("an empty trackCommands config produced output: %#v", activity)
	}
}

// ── buildSections end to end ────────────────────────────────────────────────

func TestBuildSections(t *testing.T) {
	blocks := []NormalizedBlock{
		userBlock("port the sanitizer to Go", 0, true),
		// The second message is still one of the first three, so it is an early
		// request too.
		userBlock("always use tabs", 1, true),
		{Kind: KindToolCall, Name: "edit", Args: map[string]any{"path": "/root/p/sanitize.go"}},
		// A second absolute path under the same root lets the common-prefix trim run.
		{Kind: KindToolCall, Name: "read", Args: map[string]any{"path": "/root/p/main.go"}},
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": `git commit -m "feat: sanitize"`}},
		{Kind: KindToolResult, Name: "bash", Text: "[main 1111111] feat: sanitize"},
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "go test ./..."}},
		{Kind: KindAssistant, Text: "Done. The suite passes."},
	}
	data := BuildSections(BuildSectionsInput{
		Blocks:        blocks,
		TrackCommands: []string{"go"},
	})
	if !containsStr(data.EarlyRequests, "port the sanitizer to Go") {
		t.Fatalf("EarlyRequests = %v", data.EarlyRequests)
	}
	if !containsStr(data.EarlyRequests, "always use tabs") {
		t.Fatalf("EarlyRequests = %v, want the second message too", data.EarlyRequests)
	}
	if len(data.UserMessages) != 0 {
		t.Fatalf("UserMessages = %v, want none: only two messages exist", data.UserMessages)
	}
	joinedFiles := strings.Join(data.FilesAndChanges, "\n")
	if !strings.Contains(joinedFiles, "Modified: sanitize.go") || !strings.Contains(joinedFiles, "Read: main.go") {
		t.Fatalf("FilesAndChanges = %v", data.FilesAndChanges)
	}
	if !containsStr(data.Commits, "1111111: feat: sanitize") {
		t.Fatalf("Commits = %v", data.Commits)
	}
	if len(data.TrackedCommands) == 0 || !strings.Contains(data.TrackedCommands[0], "go test ./...") {
		t.Fatalf("TrackedCommands = %v", data.TrackedCommands)
	}
	if !strings.Contains(data.BriefTranscript, "[user]") || !strings.Contains(data.BriefTranscript, "[assistant]") {
		t.Fatalf("BriefTranscript = %q", data.BriefTranscript)
	}
}

// TestBuildSections_HasEarlyRequestsSkipsTheFirstMessages pins
// build-sections.ts hasEarlyRequests: with the previous summary already holding
// the early requests, this compaction's first messages are ordinary user
// messages.
func TestBuildSections_HasEarlyRequestsSkipsTheFirstMessages(t *testing.T) {
	blocks := []NormalizedBlock{
		userBlock("the original task book", 0, true),
		userBlock("a follow-up question", 1, true),
	}
	data := BuildSections(BuildSectionsInput{Blocks: blocks, HasEarlyRequests: true})
	if len(data.EarlyRequests) != 0 {
		t.Fatalf("EarlyRequests = %v, want none", data.EarlyRequests)
	}
	if len(data.UserMessages) != 2 {
		t.Fatalf("UserMessages = %v, want both messages", data.UserMessages)
	}
}

// TestBuildSections_EarlyRequestsCapAtThree pins EARLY_REQUESTS_KEPT.
func TestBuildSections_EarlyRequestsCapAtThree(t *testing.T) {
	blocks := []NormalizedBlock{
		userBlock("first request", 0, true),
		userBlock("second request", 1, true),
		userBlock("third request", 2, true),
		userBlock("fourth request", 3, true),
	}
	data := BuildSections(BuildSectionsInput{Blocks: blocks})
	if len(data.EarlyRequests) != 3 || data.EarlyRequests[0] != "first request" {
		t.Fatalf("EarlyRequests = %v, want the first three", data.EarlyRequests)
	}
	if !containsStr(data.UserMessages, "fourth request") {
		t.Fatalf("UserMessages = %v, want the fourth request", data.UserMessages)
	}
}

// TestBuildSections_InjectedStaysOutOfBothSections pins the injected filter end
// to end: an extension message renders in the brief but reaches neither
// section.
func TestBuildSections_InjectedStaysOutOfBothSections(t *testing.T) {
	blocks := []NormalizedBlock{
		userBlockInjected("an extension note", 0, true, true),
		userBlock("the real task", 1, true),
	}
	data := BuildSections(BuildSectionsInput{Blocks: blocks})
	if containsStr(data.EarlyRequests, "an extension note") || containsStr(data.UserMessages, "an extension note") {
		t.Fatalf("an injected message reached a section: %v / %v", data.EarlyRequests, data.UserMessages)
	}
	if !strings.Contains(data.BriefTranscript, "an extension note") {
		t.Fatalf("the injected message is missing from the brief:\n%s", data.BriefTranscript)
	}
}

func TestBuildSections_BriefBlocksOverride(t *testing.T) {
	all := []NormalizedBlock{
		userBlock("the real task", 0, true),
		userBlock("<analysis>\npreamble noise\n</analysis>", 1, true),
	}
	data := BuildSections(BuildSectionsInput{
		Blocks:      all,
		BriefBlocks: all[:1],
	})
	if strings.Contains(data.BriefTranscript, "analysis") {
		t.Fatalf("BriefBlocks was ignored:\n%s", data.BriefTranscript)
	}
	if !strings.Contains(data.BriefTranscript, "the real task") {
		t.Fatalf("BriefBlocks content missing:\n%s", data.BriefTranscript)
	}
}

// TestExtractCommits_CombinedFlag pins the 0.11.1 COMMIT_MSG_RE: a combined flag
// such as -am or -qam still matches.
func TestExtractCommits_CombinedFlag(t *testing.T) {
	got := ExtractCommits([]NormalizedBlock{
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": `git commit -am "fix: amend combined flag"`}},
		{Kind: KindToolResult, Name: "bash", Text: "[main abc1234] fix: amend combined flag"},
	})
	if len(got) != 1 || got[0].Message != "fix: amend combined flag" || got[0].Hash != "abc1234" {
		t.Fatalf("ExtractCommits(-am) = %#v", got)
	}
	got = ExtractCommits([]NormalizedBlock{
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": `git commit -qam "fix: quiet combined flag"`}},
	})
	if len(got) != 1 || got[0].Message != "fix: quiet combined flag" || got[0].HasHash {
		t.Fatalf("ExtractCommits(-qam) = %#v", got)
	}
}

// TestExtractCommits_StdinHeredoc pins the 0.11.1 `-F -` path: the message comes
// from stdin through a heredoc on the same command.
func TestExtractCommits_StdinHeredoc(t *testing.T) {
	got := ExtractCommits([]NormalizedBlock{
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "git commit -F - <<'MSG'\nfeat: read the message from stdin\nMSG"}},
	})
	if len(got) != 1 || got[0].Message != "feat: read the message from stdin" {
		t.Fatalf("ExtractCommits(-F -) = %#v", got)
	}
	// A path after -F cannot be read from the transcript, so it is not matched.
	got = ExtractCommits([]NormalizedBlock{
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": "git commit -F /tmp/msg.txt"}},
	})
	if len(got) != 0 {
		t.Fatalf("ExtractCommits(-F path) = %#v, want nothing", got)
	}
}

// TestExtractCommits_MultipleInOneCommand pins the 0.11.1 segment split: every
// `git commit` in one bash command is extracted, each with the hash of the
// output line naming its subject.
func TestExtractCommits_MultipleInOneCommand(t *testing.T) {
	got := ExtractCommits([]NormalizedBlock{
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": `git commit -m "first: one" && git commit -m "second: two"`}},
		{Kind: KindToolResult, Name: "bash", Text: "[main aaa1111] first: one\n[main bbb2222] second: two"},
	})
	if len(got) != 2 {
		t.Fatalf("ExtractCommits(two commits) = %#v, want two", got)
	}
	if got[0].Message != "first: one" || got[0].Hash != "aaa1111" {
		t.Fatalf("first commit = %#v", got[0])
	}
	if got[1].Message != "second: two" || got[1].Hash != "bbb2222" {
		t.Fatalf("second commit = %#v", got[1])
	}
}

// TestExtractCommits_ProseDoesNotSplit pins the command-boundary rule: a message
// that says "git commit" is prose, not a second commit.
func TestExtractCommits_ProseDoesNotSplit(t *testing.T) {
	got := ExtractCommits([]NormalizedBlock{
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": `git commit -m "fix: mention git commit in prose"`}},
		{Kind: KindToolResult, Name: "bash", Text: "[main abc1234] fix: mention git commit in prose"},
	})
	if len(got) != 1 {
		t.Fatalf("ExtractCommits = %#v, want one commit", got)
	}
}

// TestExtractCommits_HashlessAndHashedAreOneCommit pins the dedup: the same
// message seen hashless (`-q` prints no hash) and hashed is one commit, and the
// hashed one wins — in the slot of the first occurrence, not at the tail. An
// unrelated commit in between keeps its own entry, and a later hashless repeat
// of the first message does not drop the hash it already carries.
func TestExtractCommits_HashlessAndHashedAreOneCommit(t *testing.T) {
	got := ExtractCommits([]NormalizedBlock{
		// `-q` prints nothing, so the quiet commit's own result carries no hash
		// and the result two blocks on cannot be read as its hash.
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": `git commit -q -m "fix: quiet"`}},
		{Kind: KindToolResult, Name: "bash", Text: ""},
		// An unrelated hashed commit between the two "fix: quiet" calls.
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": `git commit -m "chore: middle"`}},
		{Kind: KindToolResult, Name: "bash", Text: "[main def5678] chore: middle"},
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": `git commit -m "fix: quiet"`}},
		{Kind: KindToolResult, Name: "bash", Text: "[main abc1234] fix: quiet"},
		// A hashless repeat of the first message must not downgrade it.
		{Kind: KindToolCall, Name: "bash", Args: map[string]any{"command": `git commit -q -m "fix: quiet"`}},
	})
	if len(got) != 2 {
		t.Fatalf("ExtractCommits = %#v, want two commits", got)
	}
	if !got[0].HasHash || got[0].Hash != "abc1234" || got[0].Message != "fix: quiet" {
		t.Fatalf("first commit = %#v, want the hashed fix: quiet upgraded in place", got[0])
	}
	if !got[1].HasHash || got[1].Hash != "def5678" || got[1].Message != "chore: middle" {
		t.Fatalf("second commit = %#v, want the chore: middle entry", got[1])
	}
}

// TestExtractFiles_MultiPath pins files.ts's extractPaths loop: a multi-file edit
// tool names its files in args.paths.
func TestExtractFiles_MultiPath(t *testing.T) {
	activity := ExtractFiles([]NormalizedBlock{
		{Kind: KindToolCall, Name: "edit", Args: map[string]any{
			"code": "rewrite both", "paths": []any{"/root/p/a.go", "/root/p/b.go"},
		}},
	}, nil)
	if !activity.Modified.Has("a.go") || !activity.Modified.Has("b.go") {
		t.Fatalf("Modified = %v, want both paths", activity.Modified.Items())
	}
}

// TestExtractUserMessages_InjectedSurvivesNormalize pins the 0.11.0 injected
// marker end to end: normalize.ts reads it off the raw message, filter-noise.ts
// keeps it, and the section extraction leaves the message out.
func TestExtractUserMessages_InjectedSurvivesNormalize(t *testing.T) {
	blocks := FilterNoise(Normalize([]map[string]any{
		{"role": "user", "content": "the real task: port the extractor to Go", "injected": true},
		{"role": "user", "content": "a second real message about the tests"},
	}, nil))
	got := ExtractUserMessages(blocks)
	if len(got) != 1 || got[0] != "a second real message about the tests" {
		t.Fatalf("ExtractUserMessages = %#v, want only the user's own message", got)
	}
	// The injected message still renders in the brief transcript.
	data := BuildSections(BuildSectionsInput{Blocks: blocks})
	if !strings.Contains(data.BriefTranscript, "the real task: port the extractor to Go") {
		t.Fatalf("the injected message is missing from the brief:\n%s", data.BriefTranscript)
	}
}
