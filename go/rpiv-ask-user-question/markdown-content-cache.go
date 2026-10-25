package rpivaskuserquestion

// Ports view/components/preview/markdown-content-cache.ts (v2.15.0): the
// per-question, width-keyed cache for rendered markdown previews.

import (
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// Preview height caps and copy.
const (
	// MaxPreviewHeightSideBySide is the CC-parity cap for side-by-side.
	MaxPreviewHeightSideBySide = 20
	// MaxPreviewHeightStacked preserves narrow-terminal protection.
	MaxPreviewHeightStacked = 15
	// NoPreviewText is the placeholder when an option carries no preview.
	NoPreviewText = "No preview available"
	// NotesAffordanceOverhead is the 1 blank separator + 1 affordance row
	// reserved when the question has any preview.
	NotesAffordanceOverhead = 2
)

// cachedMarkdown is one option's markdown renderer plus its invalidation.
type cachedMarkdown struct {
	text     string
	rendered map[int][]string
}

// MarkdownContentCache caches rendered previews per option, keyed by width.
type MarkdownContentCache struct {
	theme       sdk.UITheme
	previewText map[int]string
	cache       map[int]*cachedMarkdown
	cachedWidth int
	hasWidth    bool
}

// NewMarkdownContentCache indexes the question's preview-carrying options.
func NewMarkdownContentCache(question QuestionData, theme sdk.UITheme) *MarkdownContentCache {
	texts := make(map[int]string, len(question.Options))
	for i, option := range question.Options {
		if text := option.previewText(); text != "" {
			texts[i] = text
		}
	}
	return &MarkdownContentCache{theme: theme, previewText: texts, cache: map[int]*cachedMarkdown{}}
}

// HasAnyPreview mirrors hasAnyPreview.
func (c *MarkdownContentCache) HasAnyPreview() bool { return len(c.previewText) > 0 }

// Has mirrors has(optionIndex).
func (c *MarkdownContentCache) Has(optionIndex int) bool {
	_, ok := c.previewText[optionIndex]
	return ok
}

// BodyFor mirrors bodyFor: the body lines for an option at an inner width.
// A width change invalidates every cached render.
func (c *MarkdownContentCache) BodyFor(optionIndex, innerWidth int) []string {
	if !c.hasWidth || c.cachedWidth != innerWidth {
		for _, entry := range c.cache {
			entry.rendered = map[int][]string{}
		}
		c.cachedWidth = innerWidth
		c.hasWidth = true
	}
	text, ok := c.previewText[optionIndex]
	if !ok {
		placeholder := c.theme.Fg("dim", T("preview.no_preview", NoPreviewText))
		return []string{fillWidth(placeholder, innerWidth)}
	}
	entry, ok := c.cache[optionIndex]
	if !ok {
		entry = &cachedMarkdown{text: text, rendered: map[int][]string{}}
		c.cache[optionIndex] = entry
	}
	lines, cached := entry.rendered[innerWidth]
	if !cached {
		lines = renderMarkdown(c.theme, entry.text, innerWidth)
		entry.rendered[innerWidth] = lines
	}
	return StripFenceMarkers(lines)
}

// Invalidate drops every cached render.
func (c *MarkdownContentCache) Invalidate() {
	for _, entry := range c.cache {
		entry.rendered = map[int][]string{}
	}
	c.cachedWidth = 0
	c.hasWidth = false
}

// renderMarkdown renders markdown to terminal lines at the given width.
func renderMarkdown(theme sdk.UITheme, text string, width int) []string {
	return newMarkdownRenderer(theme).render(text, width)
}

// splitLines is strings.Split for "\n", kept for readability at call sites.
func splitLines(text string) []string { return strings.Split(text, "\n") }
