package herdsman

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Pi-side reader for the `wake` section of `$HERDSMAN_HOME/config.yaml`.
//
// The Herdsman daemon validates `config.yaml` with its Runtime schema, but the
// Pi extension must not import a YAML parser, so only the documented subset is
// parsed:
//
//	wake:
//	  filter_upstream_errors: false
//	  extra_upstream_error_patterns:
//	    - "overloaded"
//	    - /foo\s+bar/i
//
// Supported subset: the top-level `wake:` mapping, 2-space indentation,
// `true`/`false` booleans, `- item` lists, `#` line comments and quoted
// strings. Anything else falls back to the defaults with a warning in the
// Herdsman Pi log; the extension is never blocked by a malformed file.
//
// Environment overrides win over the file so operators (and tests) can flip the
// filter without editing YAML.
const (
	wakeSectionKey      = "wake"
	filterKey           = "filter_upstream_errors"
	extraPatternsKey    = "extra_upstream_error_patterns"
	filterEnv           = "HERDSMAN_WAKE_FILTER_UPSTREAM_ERRORS"
	extraPatternsEnv    = "HERDSMAN_WAKE_EXTRA_UPSTREAM_ERROR_PATTERNS"
	defaultHomeName     = ".herdsman"
	delimitedExtraRegex = `^/(.+)/([gimsuy]*)$`
)

// wakeFileValues mirrors the TS `WakeFileValues`.
type wakeFileValues struct {
	extraPatterns        []string
	filterUpstreamErrors bool
}

// wakeFileParse mirrors the TS `WakeFileParse` union: absent, ok, or an error
// message.
type wakeFileParse struct {
	present bool
	ok      bool
	value   wakeFileValues
	message string
}

// extraDelimitedPattern mirrors DELIMITED_EXTRA_PATTERN.
var extraDelimitedPattern = regexp.MustCompile(delimitedExtraRegex)

// defaultHerdsmanHome mirrors the TS `defaultHerdsmanHome`. Note the difference
// from the logger's helper: this one falls back to `homedir()/.herdsman` and
// treats a non-absolute HERDSMAN_HOME as unset, which is what the TS does here.
func defaultHerdsmanHome() string {
	if configured := strings.TrimSpace(os.Getenv("HERDSMAN_HOME")); configured != "" && filepath.IsAbs(configured) {
		return configured
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "/root"
	}
	return filepath.Join(home, defaultHomeName)
}

// warnWakeFilter mirrors the TS `warn` helper: the message carries the same
// `[herdsman] ` prefix.
func warnWakeFilter(message string) { logWarn("[herdsman] " + message) }

// stripComment mirrors the TS `stripComment`: everything from an unquoted `#`.
func stripComment(line string) string {
	var quote rune
	hasQuote := false
	runes := []rune(line)
	for index := 0; index < len(runes); index++ {
		char := runes[index]
		if hasQuote {
			if char == quote {
				hasQuote = false
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			hasQuote = true
			continue
		}
		if char == '#' {
			return string(runes[:index])
		}
	}
	return line
}

// indentOf mirrors the TS `indentOf`: the count of leading spaces.
func indentOf(line string) int {
	indent := 0
	for indent < len(line) && line[indent] == ' ' {
		indent += 1
	}
	return indent
}

// unquote mirrors the TS `unquote`: a fully quoted scalar is unquoted, an empty
// scalar is the empty string, a half-quoted scalar is invalid.
func unquote(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", true
	}
	quoted := (strings.HasPrefix(trimmed, `"`) && strings.HasSuffix(trimmed, `"`)) ||
		(strings.HasPrefix(trimmed, "'") && strings.HasSuffix(trimmed, "'"))
	if quoted && len([]rune(trimmed)) >= 2 {
		runes := []rune(trimmed)
		return string(runes[1 : len(runes)-1]), true
	}
	if strings.HasPrefix(trimmed, `"`) || strings.HasPrefix(trimmed, "'") {
		return "", false
	}
	return trimmed, true
}

// parseBooleanScalar mirrors the TS `parseBooleanScalar`.
func parseBooleanScalar(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

// parseWakeSection mirrors the TS `parseWakeSection`.
func parseWakeSection(source string) wakeFileParse {
	lines := splitLines(source)
	sectionStart := -1
	for index, line := range lines {
		if strings.TrimSpace(stripComment(line)) == wakeSectionKey+":" {
			sectionStart = index
			break
		}
	}
	if sectionStart < 0 {
		return wakeFileParse{present: false}
	}

	value := wakeFileValues{
		extraPatterns:        []string{},
		filterUpstreamErrors: DefaultWakeFilterConfig.Enabled,
	}
	currentListKey := ""

	for index := sectionStart + 1; index < len(lines); index++ {
		rawLine := stripComment(lines[index])
		if strings.TrimSpace(rawLine) == "" {
			continue
		}
		indent := indentOf(rawLine)
		if indent == 0 {
			break
		}
		if strings.Contains(rawLine, "\t") {
			return wakeFileParse{present: true, ok: false, message: "tab indentation is not supported (line " + formatInt(int64(index+1)) + ")"}
		}
		body := strings.TrimSpace(rawLine)

		if strings.HasPrefix(body, "-") {
			// List items may align with the key (2 spaces) or nest one level
			// deeper (4 spaces), which is how most hand-written configs indent them.
			if indent != 2 && indent != 4 {
				return wakeFileParse{present: true, ok: false, message: "unexpected list indentation (line " + formatInt(int64(index+1)) + ")"}
			}
			if currentListKey != extraPatternsKey {
				return wakeFileParse{present: true, ok: false, message: "unexpected list item (line " + formatInt(int64(index+1)) + ")"}
			}
			item, valid := unquote(strings.TrimPrefix(body, "-"))
			if !valid || item == "" {
				return wakeFileParse{present: true, ok: false, message: "invalid list item (line " + formatInt(int64(index+1)) + ")"}
			}
			value.extraPatterns = append(value.extraPatterns, item)
			continue
		}

		if indent != 2 {
			return wakeFileParse{present: true, ok: false, message: "expected 2-space indentation (line " + formatInt(int64(index+1)) + ")"}
		}

		separator := strings.Index(body, ":")
		if separator <= 0 {
			return wakeFileParse{present: true, ok: false, message: "invalid mapping entry (line " + formatInt(int64(index+1)) + ")"}
		}
		key := strings.TrimSpace(body[:separator])
		rawValue := body[separator+1:]

		if key == filterKey {
			normalized := strings.TrimSpace(rawValue)
			if normalized == "[]" || normalized == "{}" {
				return wakeFileParse{present: true, ok: false, message: "invalid boolean for " + filterKey + " (line " + formatInt(int64(index+1)) + ")"}
			}
			parsed, valid := parseBooleanScalar(normalized)
			if !valid {
				return wakeFileParse{present: true, ok: false, message: "invalid boolean for " + filterKey + " (line " + formatInt(int64(index+1)) + ")"}
			}
			value.filterUpstreamErrors = parsed
			currentListKey = key
			continue
		}
		if key == extraPatternsKey {
			normalized := strings.TrimSpace(rawValue)
			if normalized == "[]" {
				value.extraPatterns = []string{}
				currentListKey = ""
				continue
			}
			if normalized != "" {
				return wakeFileParse{present: true, ok: false, message: "inline " + extraPatternsKey + " values are not supported (line " + formatInt(int64(index+1)) + ")"}
			}
			currentListKey = key
			continue
		}
		currentListKey = ""
	}

	return wakeFileParse{present: true, ok: true, value: value}
}

// splitLines mirrors JS `source.split(/\r?\n/)`: a trailing newline yields a
// final empty element, and CRLF is folded.
func splitLines(source string) []string {
	normalized := crlfPattern.ReplaceAllString(source, "\n")
	return strings.Split(normalized, "\n")
}

// parseFilterEnv mirrors the TS `parseFilterEnv`.
func parseFilterEnv(value *string) *bool {
	if value == nil {
		return nil
	}
	normalized := strings.ToLower(strings.TrimSpace(*value))
	switch normalized {
	case "true", "1":
		enabled := true
		return &enabled
	case "false", "0":
		enabled := false
		return &enabled
	}
	warnWakeFilter(filterEnv + " has an unsupported value; ignoring it")
	return nil
}

// parseExtraPatternsEnv mirrors the TS `parseExtraPatternsEnv`.
func parseExtraPatternsEnv(value *string) []string {
	if value == nil {
		return nil
	}
	var parts []string
	if strings.Contains(*value, "\n") {
		parts = strings.Split(*value, "\n")
	} else {
		parts = strings.Split(*value, ",")
	}
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			kept = append(kept, trimmed)
		}
	}
	return kept
}

// extraPatternWarning mirrors the TS `extraPatternWarning`.
func extraPatternWarning(candidates []string) *string {
	for _, candidate := range candidates {
		delimited := extraDelimitedPattern.FindStringSubmatch(strings.TrimSpace(candidate))
		if delimited == nil {
			continue
		}
		if _, err := compileJSRegexp(delimited[1], delimited[2]); err != nil {
			message := "invalid extra upstream error pattern " + candidate
			return &message
		}
	}
	return nil
}

// LoadWakeFilterConfig mirrors the TS `loadWakeFilterConfig`. The environment
// map is passed in (nil means os.Environ) so tests can drive it.
func LoadWakeFilterConfig(environment map[string]*string) WakeFilterConfig {
	env := environment
	if env == nil {
		env = currentEnvironment()
	}
	config := WakeFilterConfig{
		Enabled:       DefaultWakeFilterConfig.Enabled,
		ExtraPatterns: []string{},
	}

	configPath := filepath.Join(defaultHerdsmanHome(), "config.yaml")
	source, readErr := readFileIfPresent(configPath)
	if readErr != nil {
		if !errors.Is(readErr, os.ErrNotExist) {
			warnWakeFilter("could not read " + configPath + " for wake filter config; using defaults (" + readErr.Error() + ")")
		}
	}

	if source != nil {
		parsed := parseWakeSection(*source)
		switch {
		case parsed.present && parsed.ok:
			config = WakeFilterConfig{
				Enabled:       parsed.value.filterUpstreamErrors,
				ExtraPatterns: append([]string{}, parsed.value.extraPatterns...),
			}
		case parsed.present:
			warnWakeFilter("invalid wake filter config in " + configPath + ": " + parsed.message + "; using defaults")
		}
	}

	if envFilter := parseFilterEnv(env[filterEnv]); envFilter != nil {
		config.Enabled = *envFilter
	}
	if envPatterns := parseExtraPatternsEnv(env[extraPatternsEnv]); envPatterns != nil {
		config.ExtraPatterns = envPatterns
	}

	if invalidPattern := extraPatternWarning(config.ExtraPatterns); invalidPattern != nil {
		warnWakeFilter(*invalidPattern + "; it will never match")
	}

	return config
}

// currentEnvironment snapshots os.Environ into the lookup map the loader takes.
func currentEnvironment() map[string]*string {
	environment := map[string]*string{}
	for _, entry := range os.Environ() {
		key, value, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		copied := value
		environment[key] = &copied
	}
	for _, key := range []string{filterEnv, extraPatternsEnv} {
		if _, exists := environment[key]; !exists {
			environment[key] = nil
		}
	}
	return environment
}

// readFileIfPresent returns the file contents, or nil when it does not exist,
// together with the error the caller may want to report.
func readFileIfPresent(path string) (*string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := string(contents)
	return &text, nil
}
