package rpivaskuserquestion

// Shared test fixtures for the view-layer tests.

import (
	"reflect"
	"testing"
	"unsafe"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// themeForTests is the SDK's zero-value theme: per the SDK docs, "the zero value
// styles text with modifiers only, like a runtime that has not yet received a
// palette". Fg/Bg on it return the text uncolored instead of panicking (the
// unknown-token panic is only armed once a palette exists), so view tests can
// assert on plain strings while still exercising the real UITheme methods.
func themeForTests() sdk.UITheme {
	return sdk.UITheme{}
}

// paletteTheme builds a theme carrying ONLY the given foreground tokens.
//
// The SDK decodes a palette from the host, so `UITheme`'s token maps are private
// and the zero value cannot tell "styled" and "not styled" apart (it hands the text
// back unchanged). Reflection writes the field directly, which lets a test assert
// the real escape sequences Theme.fg produces. Any other token then reads as
// unknown and panics, so this theme is only for render paths that touch exactly
// these tokens.
func paletteTheme(t *testing.T, foregrounds map[string]string) sdk.UITheme {
	t.Helper()
	theme := sdk.UITheme{}
	field := reflect.ValueOf(&theme).Elem().FieldByName("foregrounds")
	if !field.IsValid() || !field.CanAddr() {
		t.Fatal("sdk.UITheme has no addressable foregrounds field")
	}
	reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Set(reflect.ValueOf(foregrounds))
	return theme
}
