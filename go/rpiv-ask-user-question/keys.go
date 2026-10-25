package rpivaskuserquestion

// Key matching for the questionnaire.
//
// The npm original calls pi-tui's `matchesKey(data, keyId)` and the host's
// keybindings manager. PiG ships that same layer (sdk/tui is a port of pi-tui),
// so the port delegates to it rather than re-implementing the KeyId grammar:
//
//   - [MatchesKey]      → sdktui.MatchesKey (legacy sequences, Kitty CSI-u and
//     modifyOtherKeys, all with the host's protocol state).
//   - [IsKeyRelease] / [IsKeyRepeat] → the SDK's Kitty event predicates, used to
//     ignore repeat/release so a held collapse key does not toggle repeatedly.
//   - [DefaultKeybindings] → the host's effective keybindings table, which the
//     SDK installs from the host's keybindings.json overrides.
//
// [SDKKeybindings] is therefore what the production session wires in; tests pass
// a scripted [Keybindings] fake instead.

import sdktui "github.com/MichaelKinsy/PiG/extensions/sdk/tui"

// MatchesKey mirrors pi-tui's matchesKey: does this raw terminal chunk carry the
// given key id?
func MatchesKey(data, keyID string) bool { return sdktui.MatchesKey(data, keyID) }

// IsKeyRelease mirrors pi-tui's isKeyRelease: a Kitty-protocol key RELEASE event.
func IsKeyRelease(data string) bool { return sdktui.IsKeyRelease(data) }

// IsKeyRepeat mirrors pi-tui's isKeyRepeat: a Kitty-protocol key REPEAT event.
func IsKeyRepeat(data string) bool { return sdktui.IsKeyRepeat(data) }

// SDKKeybindings drives [Keybindings] from the host's effective keybinding table,
// the equivalent of the `keybindings` argument the TS session receives from
// ctx.ui.custom.
type SDKKeybindings struct{}

// Matches reports whether data triggers the semantic keybinding name.
func (SDKKeybindings) Matches(data, name string) bool {
	return sdktui.GetKeybindings().Matches(data, name)
}

// DefaultKeybindings is the production keybinding source.
var DefaultKeybindings = SDKKeybindings{}
