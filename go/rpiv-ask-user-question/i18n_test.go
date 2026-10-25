package rpivaskuserquestion

import (
	"strings"
	"testing"
)

func withLocale(t *testing.T, locale string) {
	t.Helper()
	previous := LocaleSource
	LocaleSource = func() string { return locale }
	t.Cleanup(func() { LocaleSource = previous })
}

func TestEmbeddedLocalesLoad(t *testing.T) {
	if err := LocaleLoadError(); err != nil {
		t.Fatalf("embedded locales failed to load: %v", err)
	}
	dictionaries := LocaleDictionaries()
	if len(dictionaries) != 9 {
		t.Fatalf("locale count = %d, want 9", len(dictionaries))
	}
	for _, code := range SupportedLocales {
		if _, present := dictionaries[code]; !present {
			t.Fatalf("locale %q missing", code)
		}
	}
	want := []string{"de", "en", "es", "fr", "pt", "pt-BR", "ru", "uk", "zh"}
	if len(SupportedLocales) != len(want) {
		t.Fatalf("SupportedLocales = %v", SupportedLocales)
	}
	for i := range want {
		if SupportedLocales[i] != want[i] {
			t.Fatalf("SupportedLocales[%d] = %q, want %q", i, SupportedLocales[i], want[i])
		}
	}
}

// Every locale is a *partial* dictionary by design: the shipped translations
// carry 25 keys and rely on the bridge's English fallback for everything else.
// The two invariants that must hold are (a) no locale invents a key that en.json
// does not have, and (b) every key a locale does carry resolves to a non-empty
// string.
func TestLocaleDictionariesAreValidPartialSets(t *testing.T) {
	dictionaries := LocaleDictionaries()
	english := dictionaries["en"]
	for _, code := range SupportedLocales {
		dict := dictionaries[code]
		for key, value := range dict {
			// `_meta.*` entries are translator notes, not keys.
			if strings.HasPrefix(key, "_meta.") {
				continue
			}
			if _, known := english[key]; !known {
				t.Fatalf("locale %q defines unknown key %q", code, key)
			}
			if value == "" {
				t.Fatalf("locale %q has an empty value for %q", code, key)
			}
		}
	}
	// The English dictionary itself must carry the whole namespace.
	required := []string{
		"sentinel.other", "sentinel.next", "submit.label", "submit.cancel",
		"hint.enter", "hint.navigate", "hint.newline", "hint.clear", "hint.toggle",
		"hint.notes", "hint.tab", "hint.cancel", "hint.collapse", "hint.expand_line",
		"review.heading", "review.ready", "review.incomplete", "review.global_hint",
		"review.note_label", "preview.no_preview", "preview.notes_affordance",
		"preview.notes_edit_affordance", "notes.header", "notes.saved_label",
		"notes.global_header", "editor.failed", "rpc.multi_instructions",
		"rpc.custom_answer_title", "transcript.not_answered", "transcript.no_answer",
	}
	for _, key := range required {
		if _, present := english[key]; !present {
			t.Fatalf("en.json is missing %q", key)
		}
	}
	if len(english) < len(required) {
		t.Fatalf("en.json has %d keys, want at least %d", len(english), len(required))
	}
}

// With no locale active, T returns the inline English fallback at every call
// site — byte-identical to the npm original without rpiv-i18n.
func TestTFallsBackToEnglishWithoutLocale(t *testing.T) {
	withLocale(t, "")
	if got := T("sentinel.other", "Type something."); got != "Type something." {
		t.Fatalf("T = %q", got)
	}
	if got := T("nonexistent.key", "fallback"); got != "fallback" {
		t.Fatalf("T = %q", got)
	}
	if got := DisplayLabel(SentinelNext); got != "Next" {
		t.Fatalf("DisplayLabel = %q", got)
	}
}

func TestTTranslatesWithActiveLocale(t *testing.T) {
	withLocale(t, "zh")
	if got := T("sentinel.other", "Type something."); got != "输入内容" {
		t.Fatalf("zh sentinel.other = %q", got)
	}
	if got := T("submit.label", "Submit answers"); got != "提交答案" {
		t.Fatalf("zh submit.label = %q", got)
	}
	if got := DisplayLabel(SentinelOther); got != "输入内容" {
		t.Fatalf("DisplayLabel = %q", got)
	}

	withLocale(t, "de")
	if got := T("sentinel.next", "Next"); got != "Weiter" {
		t.Fatalf("de sentinel.next = %q", got)
	}

	withLocale(t, "pt-BR")
	if got := T("transcript.no_answer", "no answer"); got != "sem resposta" {
		t.Fatalf("pt-BR no_answer = %q", got)
	}
	// pt (Portugal) is a distinct file.
	withLocale(t, "pt")
	if got := T("transcript.no_answer", "no answer"); got != "sem resposta" {
		t.Fatalf("pt no_answer = %q", got)
	}
}

// A key present in en but missing from the active locale still falls back — the
// shipped translations are partial (25 keys) and rely on this path.
func TestTMissingKeyFallsBack(t *testing.T) {
	withLocale(t, "de")
	if got := T("hint.collapse", "{key} to collapse"); got != "{key} to collapse" {
		t.Fatalf("T = %q", got)
	}
	if got := T("review.global_hint", "{key} to add a note"); got != "{key} to add a note" {
		t.Fatalf("T = %q", got)
	}
	if got := T("totally.unknown", "inline"); got != "inline" {
		t.Fatalf("T = %q", got)
	}
}

// {key} placeholders survive translation verbatim so call sites interpolate.
func TestLocaleTemplatesKeepKeyPlaceholder(t *testing.T) {
	dictionaries := LocaleDictionaries()
	for _, code := range SupportedLocales {
		for key, value := range dictionaries[code] {
			// Only templates carry `{key}` in the English source.
			english := dictionaries["en"][key]
			wantsPlaceholder := containsSubstring(english, KeyPlaceholder)
			hasPlaceholder := containsSubstring(value, KeyPlaceholder)
			if wantsPlaceholder && !hasPlaceholder {
				t.Fatalf("locale %q key %q lost the %s placeholder", code, key, KeyPlaceholder)
			}
		}
	}
}

func TestHasLocale(t *testing.T) {
	for _, code := range SupportedLocales {
		if !HasLocale(code) {
			t.Fatalf("HasLocale(%q) = false", code)
		}
	}
	for _, code := range []string{"", "en-US", "ja", "EN"} {
		if HasLocale(code) {
			t.Fatalf("HasLocale(%q) = true", code)
		}
	}
}

func TestNormalizeLocaleCode(t *testing.T) {
	cases := map[string]string{
		"de_DE.UTF-8": "de",
		"de":          "de",
		"pt_BR":       "pt-BR",
		"pt-BR":       "pt-BR",
		"pt_PT":       "pt",
		"ru_RU.utf8":  "ru",
		"zh_CN.UTF-8": "zh",
		"C":           "",
		"POSIX":       "",
		"ja_JP":       "",
		"":            "",
		"en@quot":     "en",
	}
	for raw, want := range cases {
		if got := normalizeLocaleCode(raw); got != want {
			t.Fatalf("normalizeLocaleCode(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestLookupIn(t *testing.T) {
	dictionaries := LocaleDictionaries()
	if got := lookupIn(dictionaries, "", "sentinel.next"); got != "" {
		t.Fatalf("lookup with no locale = %q", got)
	}
	if got := lookupIn(dictionaries, "fr", "sentinel.next"); got != "Suivant" {
		t.Fatalf("fr sentinel.next = %q", got)
	}
	if got := lookupIn(dictionaries, "nope", "sentinel.next"); got != "" {
		t.Fatalf("unknown locale = %q", got)
	}
	if got := lookupIn(dictionaries, "fr", "missing"); got != "" {
		t.Fatalf("missing key = %q", got)
	}
}

// localeFromEnv resolves a locale from the process environment variables.
func TestLocaleFromEnv(t *testing.T) {
	clear := func() {
		for _, key := range []string{"RPIV_I18N_LOCALE", "PIG_LOCALE", "LANG", "LC_ALL", "LC_MESSAGES"} {
			t.Setenv(key, "")
		}
	}

	clear()
	t.Setenv("LANG", "de_DE.UTF-8")
	if got := localeFromEnv(); got != "de" {
		t.Fatalf("localeFromEnv = %q", got)
	}

	clear()
	t.Setenv("LC_ALL", "zh_CN.UTF-8")
	if got := localeFromEnv(); got != "zh" {
		t.Fatalf("localeFromEnv = %q", got)
	}

	clear()
	t.Setenv("RPIV_I18N_LOCALE", "fr")
	t.Setenv("LANG", "de_DE.UTF-8")
	if got := localeFromEnv(); got != "fr" {
		t.Fatalf("RPIV_I18N_LOCALE ignored: %q", got)
	}

	clear()
	if got := localeFromEnv(); got != "" {
		t.Fatalf("localeFromEnv with no vars = %q", got)
	}
}

// The localized sentinel label is display-only; every view that renders a
// sentinel row goes through DisplayLabel.
func TestDisplayLabelUnknownKind(t *testing.T) {
	if got := DisplayLabel(SentinelKind("bogus")); got != "" {
		t.Fatalf("DisplayLabel(bogus) = %q", got)
	}
}
