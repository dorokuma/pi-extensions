package compactui

import (
	"os"
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// This file ports src/settings.ts (L2084-2191): the CONFIG_KEYS table, the
// makeStepper component and the /compact-ui-config command.

// configKey describes one editable setting (CONFIG_KEYS, L2084-2095).
type configKey struct {
	id          string
	label       string
	description string
	min         int
	max         int
	step        int
}

// configKeys is CONFIG_KEYS.
var configKeys = []configKey{
	{
		id:          configField,
		label:       "Max collapsed entries",
		description: "Maximum entries shown in collapsed group",
		min:         1,
		max:         30,
		step:        1,
	},
}

// stepperComponent is makeStepper's returned component (L2196-2234): a bar
// that shows one numeric setting and commits or cancels it.
type stepperComponent struct {
	title   string
	value   int
	meta    configKey
	th      theme
	done    func(*string)
	width   int
	cached  []string
	cachedW int
	hasCash bool
}

// makeStepper is makeStepper (L2196-2234).
func makeStepper(title string, initial int, meta configKey, th theme, done func(*string)) *stepperComponent {
	return &stepperComponent{title: title, value: initial, meta: meta, th: th, done: done, cachedW: -1}
}

// renderWidth is makeStepper's render (L2203-2223).
func (s *stepperComponent) renderWidth(width int) []string {
	if s.hasCash && s.cachedW == width {
		return s.cached
	}
	barLen := maxInt(1, minInt(width-10, 40))
	span := maxInt(1, s.meta.max-s.meta.min)
	ratio := float64(s.value-s.meta.min) / float64(span)
	filled := int(roundHalfUp(ratio * float64(barLen)))
	if filled < 0 {
		filled = 0
	}
	if filled > barLen {
		filled = barLen
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", maxInt(0, barLen-filled))
	titleText := s.title
	if s.th != nil {
		titleText = s.th.bold(s.title)
	}
	lines := []string{
		fgOf(s.th, "accent", titleText),
		"",
		"  " + fgOf(s.th, "accent", itoa(s.value)),
		"  " + fgOf(s.th, "muted", bar),
		"",
		fgOf(s.th, "dim", "  ◀ ▶ / − +  adjust    Enter  save    Esc  cancel"),
	}
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = truncateToWidthEllipsis(line, maxInt(1, width), "")
	}
	s.cached = out
	s.cachedW = width
	s.hasCash = true
	return out
}

// handleInput is makeStepper's handleInput (L2210-2226).
func (s *stepperComponent) handleInput(data string) {
	switch {
	case matchesKey(data, "left") || matchesKey(data, "down") || data == "-" || data == "_":
		s.value = maxInt(s.meta.min, s.value-s.meta.step)
	case matchesKey(data, "right") || matchesKey(data, "up") || data == "+" || data == "=":
		s.value = minInt(s.meta.max, s.value+s.meta.step)
	case matchesKey(data, "enter") || matchesKey(data, "space"):
		value := itoa(s.value)
		s.done(&value)
		return
	case matchesKey(data, "escape"):
		s.done(nil)
		return
	}
	s.invalidate()
}

// invalidate is makeStepper's invalidate (L2228-2230).
func (s *stepperComponent) invalidate() {
	s.cachedW = -1
	s.hasCash = false
}

// Render implements remoteComponent.
func (s *stepperComponent) Render(width int) []string { return s.renderWidth(width) }

// HandleInput implements remoteComponent.
func (s *stepperComponent) HandleInput(data string) (bool, any) {
	s.handleInput(data)
	return true, nil
}

// registerConfigCommand is registerConfigCommand (L2235-2287): the
// /compact-ui-config command. Without a UI it notifies the current value; with
// one it opens the interactive settings list and persists a change.
func (r *runtimeState) registerConfigCommand(e *sdk.Extension) {
	e.Command("compact-ui-config", "Interactive compact-ui settings (arrows to select, Enter to adjust, Esc to close)", func(ctx sdk.Context, _ string) error {
		r.handleConfigCommand(ctx)
		return nil
	})
}

// handleConfigCommand is the command handler body. The host call (Notify /
// Custom) runs outside mu; the state write is inside.
func (r *runtimeState) handleConfigCommand(ctx sdk.Context) {
	ui := r.uiContextFor(ctx)
	if ui == nil || !ui.hasUI() {
		r.mu.Lock()
		current := r.maxGroupEntriesLocked()
		r.mu.Unlock()
		if ui != nil {
			ui.notify("compact: "+configField+"="+itoa(current), "info")
			return
		}
		notifyHost(ctx, "compact: "+configField+"="+itoa(current), "info")
		return
	}
	r.mu.Lock()
	current := r.maxGroupEntriesLocked()
	th := r.currentTheme
	path := r.configPath
	r.mu.Unlock()
	component := newSettingsComponent(r, current, th, path)
	changed, err := ui.custom(func(tuiHandle) remoteComponent { return component }, settingsOverlayOptions{})
	if err != nil {
		return
	}
	if applied, ok := changed.(bool); ok && applied {
		ui.notify("compact-ui settings saved", "info")
	}
}

// settingsComponent is the ctx.ui.custom component of the config command
// (L2249-2280): the SettingsList over CONFIG_KEYS.
type settingsComponent struct {
	tui     *runtimeState
	items   []configKey
	values  map[string]int
	th      theme
	path    string
	index   int
	stepper *stepperComponent
	changed bool
	done    func(bool)
}

// newSettingsComponent builds the list over the current config.
func newSettingsComponent(r *runtimeState, current int, th theme, path string) *settingsComponent {
	return &settingsComponent{
		tui:    r,
		items:  configKeys,
		values: map[string]int{configField: current},
		th:     th,
		path:   path,
	}
}

// Render implements remoteComponent: the open stepper, else the item list.
func (s *settingsComponent) Render(width int) []string {
	if s.stepper != nil {
		return s.stepper.Render(width)
	}
	lines := make([]string, 0, len(s.items)+2)
	for i, item := range s.items {
		prefix := "  "
		label := item.label
		if i == s.index {
			prefix = "▶ "
			label = boldOf(s.th, item.label)
		}
		lines = append(lines, truncateToWidthEllipsis(prefix+label+"  "+fgOf(s.th, "accent", itoa(s.values[item.id])), maxInt(1, width), ""))
	}
	lines = append(lines, "", truncateToWidthEllipsis(fgOf(s.th, "dim", "  Enter  adjust    Esc  close"), maxInt(1, width), ""))
	return lines
}

// HandleInput implements remoteComponent: the settings list keys, then the
// stepper's.
func (s *settingsComponent) HandleInput(data string) (done bool, value any) {
	if s.stepper != nil {
		// Forward the key to the open stepper. It is closed only by the
		// stepper's own done callback (Enter saves, Esc cancels), as L2249-2280
		// does: clearing s.stepper here would reopen the stepper on the next key.
		s.stepper.handleInput(data)
		return false, nil
	}
	switch {
	case matchesKey(data, "escape"):
		return true, s.changed
	case matchesKey(data, "up") || data == "k":
		s.index = maxInt(0, s.index-1)
	case matchesKey(data, "down") || data == "j":
		s.index = minInt(len(s.items)-1, s.index+1)
	case matchesKey(data, "enter") || data == "\r" || data == "\n":
		item := s.items[s.index]
		s.stepper = makeStepper(item.label, s.values[item.id], item, s.th, func(chosen *string) {
			if chosen != nil {
				s.apply(item.id, *chosen)
			}
			s.stepper = nil
		})
	}
	return false, nil
}

// stepperHandle lets the settings list forward one key to the stepper.
type stepperHandle struct {
	owner   *settingsComponent
	stepper *stepperComponent
}

func (h *stepperHandle) handle(data string) {
	h.stepper.handleInput(data)
}

// apply stores one changed value and persists the config (L2263-2269).
func (s *settingsComponent) apply(id, raw string) {
	var value int
	for _, r := range raw {
		if r < '0' || r > '9' {
			return
		}
		value = value*10 + int(r-'0')
	}
	if id != configField {
		return
	}
	s.values[id] = value
	s.changed = true
	s.tui.mu.Lock()
	previous := s.tui.maxGroupEntries
	s.tui.maxGroupEntries = value
	s.tui.config = compactConfig{MaxGroupEntries: value}
	path := s.tui.configPath
	groups := s.tui.groupsListLocked()
	s.tui.mu.Unlock()
	_ = previous
	saveConfig(compactConfig{MaxGroupEntries: value}, path, writeFile)
	for _, group := range groups {
		group.invalidate()
	}
}

// settingsOverlayOptions is the (empty) ui.custom options of the config
// command: PT opens it as a plain focused component, not an overlay (L2249).
type settingsOverlayOptions struct{}

// notifyHost is a host notification with the panic guard the SDK needs for a
// zero Context.
func notifyHost(ctx sdk.Context, message, level string) {
	defer func() { _ = recover() }()
	ctx.Notify(message, level)
}

// writeFile persists bytes, returning the write error.
func writeFile(path string, payload []byte) error {
	if path == "" {
		return os.ErrInvalid
	}
	return os.WriteFile(path, payload, 0o644)
}
