package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const progName = "install.sh"

// hardNeverWrite mirrors registry.neverWrite as a defense-in-depth hardcoded denylist.
var hardNeverWrite = []string{"herdr-agent-state.ts"}

// Registry models federated/registry.json.
type Registry struct {
	SchemaVersion  int         `json:"schemaVersion"`
	DefaultDestDir string      `json:"defaultDestDir"`
	SettingsJSON   string      `json:"settingsJson"`
	NeverWrite     []string    `json:"neverWrite"`
	Extensions     []Extension `json:"extensions"`
}

// Extension represents a single registered extension entry.
type Extension struct {
	Name       string    `json:"name"`
	Category   string    `json:"category"`
	Managed    string    `json:"managed"`
	EntryKind  string    `json:"entryKind,omitempty"`
	DestFile   string    `json:"destFile,omitempty"`
	DestName   string    `json:"destName,omitempty"`
	TargetPath string    `json:"targetPath,omitempty"`
	DeployedAs *string   `json:"deployedAs,omitempty"`
	ReadOnly   bool      `json:"readOnly,omitempty"`
	Note       string    `json:"note,omitempty"`
	Authority  Authority `json:"authority"`
}

// Authority describes the authoritative source for an extension.
type Authority struct {
	Kind           string  `json:"kind"`
	SourcePath     *string `json:"sourcePath"`
	HostRepo       *string `json:"hostRepo"`
	InstallScript  *string `json:"installScript"`
	InstallDestEnv *string `json:"installDestEnv"`
}

func (a Authority) sourcePath() string {
	if a.SourcePath == nil {
		return ""
	}
	return *a.SourcePath
}

func (a Authority) installScript() string {
	if a.InstallScript == nil {
		return ""
	}
	return *a.InstallScript
}

func (a Authority) installDestEnv() string {
	if a.InstallDestEnv == nil {
		return ""
	}
	return *a.InstallDestEnv
}

func (e Extension) effectiveCategory() string {
	if e.Category == "" {
		return "hosted"
	}
	return e.Category
}

func (e Extension) effectiveDestName() string {
	if e.DestName != "" {
		return e.DestName
	}
	return e.Name
}

func loadRegistry(path string) (*Registry, error) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return nil, fmt.Errorf("registry not found: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read registry: %s: %w", path, err)
	}
	var reg Registry
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("registry is not valid JSON: %s", path)
	}
	return &reg, nil
}

func (r *Registry) find(name string) (*Extension, bool) {
	for i := range r.Extensions {
		if r.Extensions[i].Name == name {
			return &r.Extensions[i], true
		}
	}
	return nil, false
}

func (r *Registry) registeredDestNames() map[string]struct{} {
	set := make(map[string]struct{}, len(r.Extensions))
	for _, ext := range r.Extensions {
		if ext.DestFile != "" {
			set[ext.DestFile] = struct{}{}
		} else if ext.DestName != "" {
			set[ext.DestName] = struct{}{}
		}
	}
	return set
}

// installableExtensions returns hosted/federated entries filtered against hardNeverWrite by destFile.
func (r *Registry) installableExtensions() []Extension {
	deny := make(map[string]struct{}, len(hardNeverWrite))
	for _, f := range hardNeverWrite {
		deny[f] = struct{}{}
	}
	seen := make(map[string]struct{}, len(r.Extensions))
	out := make([]Extension, 0, len(r.Extensions))
	for _, ext := range r.Extensions {
		cat := ext.effectiveCategory()
		if cat != "hosted" && cat != "federated" {
			continue
		}
		if _, blocked := deny[ext.DestFile]; blocked {
			continue
		}
		if _, dup := seen[ext.Name]; dup {
			continue
		}
		seen[ext.Name] = struct{}{}
		out = append(out, ext)
	}
	return out
}

func isNeverWrite(destFile string) bool {
	for _, nw := range hardNeverWrite {
		if destFile == nw {
			return true
		}
	}
	return false
}

// md5OfRegular computes hex MD5 only on regular non-symlink files (lstat guard prevents FIFO/device hangs).
func md5OfRegular(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func filePermsOctal(path string) string {
	info, err := os.Lstat(path)
	if err != nil {
		return "(err)"
	}
	return fmt.Sprintf("%o", info.Mode().Perm())
}

// realpathMissing mirrors GNU `realpath -m`: resolves symlinks on the longest
// existing ancestor and lexically cleans any trailing missing segments.
func realpathMissing(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved), nil
	}
	cur := abs
	var tail []string
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			parts := append([]string{resolved}, tail...)
			return filepath.Clean(filepath.Join(parts...)), nil
		}
	}
}

func pathExistsOrSymlink(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func isRegularNoFollow(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

func isAbsPath(p string) bool {
	if p == "" {
		return false
	}
	return filepath.IsAbs(p) || strings.HasPrefix(p, "/")
}
