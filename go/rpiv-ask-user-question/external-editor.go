package rpivaskuserquestion

// Ports state/external-editor.ts (v2.15.0): edit a custom answer with the
// configured external-editor command.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ExternalEditorTui is the TUI lifecycle the editor flow drives.
type ExternalEditorTui interface {
	Stop()
	Start()
	RequestRender(force bool)
}

// EditorCommandResolver returns the configured external-editor command ("" when
// none is configured).
type EditorCommandResolver func() string

// ResolveEditorCommand builds a resolver from $VISUAL / $EDITOR, matching Pi's
// settings precedence: VISUAL wins, then EDITOR.
func ResolveEditorCommand(lookup func(string) string) EditorCommandResolver {
	if lookup == nil {
		lookup = os.Getenv
	}
	return func() string {
		if visual := strings.TrimSpace(lookup("VISUAL")); visual != "" {
			return visual
		}
		return strings.TrimSpace(lookup("EDITOR"))
	}
}

// LaunchNotice is the line printed while the editor owns the terminal.
const LaunchNoticeFormat = "Launching external editor: %s\nPi will resume when the editor exits.\n"

// EditWithExternalEditor mirrors editWithExternalEditor: write the value to a
// temp .md file, hand the terminal to the editor, and read the result back with
// one trailing newline normalized away.
func EditWithExternalEditor(
	tui ExternalEditorTui,
	command, value string,
	run func(editor string, args []string, file string) error,
) (string, error) {
	tempDir, err := os.MkdirTemp("", "rpiv-ask-user-question-")
	if err != nil {
		return "", err
	}
	tempFile := filepath.Join(tempDir, "answer.md")

	// The command grammar stays identical to the built-in external-editor flow:
	// a plain space split. A separate shell or argv parser here would make
	// Ctrl+G behave differently.
	editor, args := splitEditorCommand(command)
	if editor == "" {
		cleanupTemp(tempDir)
		return "", errEmptyEditorCommand
	}
	_ = args

	writeErr := os.WriteFile(tempFile, []byte(value), 0o600)
	if writeErr != nil {
		cleanupTemp(tempDir)
		return "", writeErr
	}

	stopped := false
	if tui != nil {
		tui.Stop()
		stopped = true
	}
	defer func() {
		cleanupTemp(tempDir)
		if stopped && tui != nil {
			tui.Start()
			tui.RequestRender(true)
		}
	}()

	if run == nil {
		run = runEditorProcess
	}
	if err := run(editor, args, tempFile); err != nil {
		return "", err
	}
	edited, err := os.ReadFile(tempFile)
	if err != nil {
		return "", err
	}
	return stripOneTrailingNewline(string(edited)), nil
}

// stripOneTrailingNewline mirrors `.replace(/\r?\n$/, "")`: remove at most one
// trailing CRLF or LF.
func stripOneTrailingNewline(text string) string {
	if strings.HasSuffix(text, "\r\n") {
		return text[:len(text)-2]
	}
	if strings.HasSuffix(text, "\n") {
		return text[:len(text)-1]
	}
	return text
}

// splitEditorCommand mirrors `command.split(" ")`, dropping leading empty parts.
func splitEditorCommand(command string) (string, []string) {
	parts := strings.Split(command, " ")
	for len(parts) > 0 && parts[0] == "" {
		parts = parts[1:]
	}
	if len(parts) == 0 {
		return "", nil
	}
	return parts[0], parts[1:]
}

// runEditorProcess is the default editor runner: stdio inherited, and a shell on
// Windows (matching the TS `shell: process.platform === "win32"`).
func runEditorProcess(editor string, args []string, file string) error {
	full := append(append([]string{}, args...), file)
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", append([]string{"/C", editor}, full...)...)
	} else {
		cmd = exec.Command(editor, full...)
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if signal := exitErr.String(); signal != "" {
				return &editorExitError{reason: signal}
			}
		}
		return err
	}
	return nil
}

// editorExitError is "External editor exited with <reason>".
type editorExitError struct{ reason string }

func (e *editorExitError) Error() string { return "External editor exited with " + e.reason }

// errEmptyEditorCommand is the empty-command rejection.
var errEmptyEditorCommand = &editorExitError{reason: "External editor command is empty"}

// cleanupTemp removes the temp dir, best effort.
func cleanupTemp(dir string) {
	_ = os.RemoveAll(dir)
}
