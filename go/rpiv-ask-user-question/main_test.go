package rpivaskuserquestion

// Test isolation for the locale bridge.
//
// Production resolves the active locale from the process environment
// (RPIV_I18N_LOCALE / PIG_LOCALE / LANG / …), which is what the npm original's
// rpiv-i18n SDK does. A unit test must not depend on the machine's LANG: the
// whole package runs with no locale (so every T() call returns its inline
// English fallback, byte-identical to the npm original without rpiv-i18n), and
// individual tests opt into a locale with withLocale.
//
// This file overrides LocaleSource for the duration of the test binary only; it
// compiles into the test package, never the extension.

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	LocaleSource = func() string { return "" }
	os.Exit(m.Run())
}
