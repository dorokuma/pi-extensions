package rpivaskuserquestion

// Ports state/i18n-bridge.ts (v2.15.0) plus the embedded `locales/` tree.
//
// The TS bridge is backed by the optional `@juicesharp/rpiv-i18n` SDK and
// degrades to canonical-English fallbacks when it is absent. The Go port has no
// i18n SDK dependency; instead every locale ships embedded in the binary and the
// active locale is chosen by [SetLocale]. With no locale set (the default) `T`
// returns the inline English fallback at every call site — byte-identical to the
// npm original running without rpiv-i18n.
//
// Reserved-label validation stays English-locked: validation checks the
// canonical [RowIntentMeta] labels, never the localized [DisplayLabel].

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// I18nNamespace is the rpiv-i18n namespace string (kept for parity/logging).
const I18nNamespace = "@juicesharp/rpiv-ask-user-question"

// SupportedLocales lists the shipped locale codes in the order the SDK's
// SUPPORTED_LOCALES iterates them. Each has a matching `locales/<code>.json`.
var SupportedLocales = []string{"de", "en", "es", "fr", "pt", "pt-BR", "ru", "uk", "zh"}

//go:embed locales/*.json
var localesFS embed.FS

// localeStore holds the parsed dictionaries, loaded once.
type localeStore struct {
	once   sync.Once
	byCode map[string]map[string]string
	err    error
}

// defaultLocales is the process-wide store the package-level helpers use.
var defaultLocales = &localeStore{}

// load parses every embedded locale file. A missing or malformed file is an
// error — the build would otherwise ship a locale that silently falls back.
func (s *localeStore) load() (map[string]map[string]string, error) {
	s.once.Do(func() {
		s.byCode = make(map[string]map[string]string, len(SupportedLocales))
		for _, code := range SupportedLocales {
			raw, err := localesFS.ReadFile("locales/" + code + ".json")
			if err != nil {
				s.err = fmt.Errorf("locale %s: %w", code, err)
				return
			}
			var dict map[string]any
			if err := json.Unmarshal(raw, &dict); err != nil {
				s.err = fmt.Errorf("locale %s: %w", code, err)
				return
			}
			entries := make(map[string]string, len(dict))
			for key, value := range dict {
				if text, ok := value.(string); ok {
					entries[key] = text
				}
			}
			s.byCode[code] = entries
		}
	})
	return s.byCode, s.err
}

// LocaleSource resolves the active locale code. It is a package-level hook so
// tests can pin a locale without touching process state.
var LocaleSource = func() string { return localeFromEnv() }

// localeFromEnv reads the locale from the environment, honouring the same
// variables the rpiv-i18n SDK does.
func localeFromEnv() string {
	for _, key := range []string{"RPIV_I18N_LOCALE", "PIG_LOCALE", "LANG", "LC_ALL", "LC_MESSAGES"} {
		raw, ok := osConfigEnv().readEnvVar(key)
		if !ok {
			continue
		}
		code := normalizeLocaleCode(raw)
		if code != "" {
			return code
		}
	}
	return ""
}

// normalizeLocaleCode maps a POSIX locale string (e.g. "de_DE.UTF-8") to a
// shipped code, or "" when nothing matches.
func normalizeLocaleCode(raw string) string {
	code := raw
	if idx := strings.IndexAny(code, ".@"); idx >= 0 {
		code = code[:idx]
	}
	code = strings.ReplaceAll(code, "_", "-")
	if code == "" || strings.EqualFold(code, "C") || strings.EqualFold(code, "POSIX") {
		return ""
	}
	// Exact match first (pt-BR), then the base language (pt_BR -> pt).
	for _, supported := range SupportedLocales {
		if strings.EqualFold(supported, code) {
			return supported
		}
	}
	base := code
	if idx := strings.Index(base, "-"); idx >= 0 {
		base = base[:idx]
	}
	for _, supported := range SupportedLocales {
		if strings.EqualFold(supported, base) {
			return supported
		}
	}
	return ""
}

// HasLocale reports whether code is one of the shipped locales.
func HasLocale(code string) bool {
	_, ok := defaultLocales.mustLoad()[code]
	return ok
}

func (s *localeStore) mustLoad() map[string]map[string]string {
	dictionaries, _ := s.load()
	return dictionaries
}

// Lookup returns the translated value for key in the active locale, or "" when
// the active locale has no entry (or no locale is active).
func Lookup(key string) string {
	return lookupIn(defaultLocales.mustLoad(), LocaleSource(), key)
}

// lookupIn is Lookup with the dictionary set and locale passed in, so tests can
// drive it without the package-level store.
func lookupIn(dictionaries map[string]map[string]string, locale, key string) string {
	if locale == "" {
		return ""
	}
	dict, ok := dictionaries[locale]
	if !ok {
		return ""
	}
	return dict[key]
}

// T mirrors the bridge's `t(key, fallback)`: the active locale's entry when it
// has one, else the inline canonical-English fallback.
func T(key, fallback string) string {
	if translated := Lookup(key); translated != "" {
		return translated
	}
	return fallback
}

// DisplayLabel resolves a sentinel kind to its locale-aware label, falling back
// to the canonical English ROW_INTENT_META label so nothing renders blank.
func DisplayLabel(kind SentinelKind) string {
	meta, ok := rowIntentMeta[kind]
	if !ok {
		return ""
	}
	return T("sentinel."+string(kind), meta.Label)
}

// LocaleDictionaries returns every loaded dictionary (tests and the README's
// locale table use it).
func LocaleDictionaries() map[string]map[string]string {
	return defaultLocales.mustLoad()
}

// LocaleLoadError exposes the embedded-locale parse error, if any. The build's
// tests assert it is nil, which pins the nine files as loadable.
func LocaleLoadError() error {
	_, err := defaultLocales.load()
	return err
}
