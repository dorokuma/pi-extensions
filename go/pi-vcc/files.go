package pivcc

import "strings"

// Ports src/extract/files.ts: which files were read, modified or created.

// fileReadTools is files.ts FILE_READ_TOOLS (lowercase; matching is
// case-insensitive).
var fileReadTools = map[string]bool{
	"read": true, "read_file": true, "view": true,
}

// Multi-file patch tools (apply_patch) carry their paths inside the diff
// payload, not in a path arg, so ExtractPaths yields nothing for them; those
// files are recovered from the hook-provided fileOps below.
var fileWriteTools = map[string]bool{
	"edit": true, "write": true, "edit_file": true, "write_file": true,
	"multiedit": true, "quick_edit": true, "target_edit": true, "apply_patch": true,
}

// fileCreateTools is files.ts FILE_CREATE_TOOLS.
var fileCreateTools = map[string]bool{
	"write": true, "write_file": true,
}

// FileActivity is files.ts FileActivity. Sets keep insertion order so the
// rendered order matches the TS `Set` iteration order.
type FileActivity struct {
	Read     *orderedSet
	Modified *orderedSet
	Created  *orderedSet
}

// longestCommonDirPrefix is files.ts longestCommonDirPrefix(): the longest
// common directory prefix among absolute paths, or "".
func longestCommonDirPrefix(paths []string) string {
	absolute := make([][]string, 0, len(paths))
	for _, path := range paths {
		if strings.HasPrefix(path, "/") {
			absolute = append(absolute, strings.Split(path, "/"))
		}
	}
	if len(absolute) < 2 {
		return ""
	}
	minLen := len(absolute[0])
	for _, parts := range absolute {
		if len(parts) < minLen {
			minLen = len(parts)
		}
	}
	i := 0
	for i < minLen-1 {
		seg := absolute[0][i]
		same := true
		for _, parts := range absolute {
			if parts[i] != seg {
				same = false
				break
			}
		}
		if !same {
			break
		}
		i++
	}
	if i < 2 {
		return ""
	}
	return strings.Join(absolute[0][:i], "/") + "/"
}

// trimPaths is files.ts trimPaths().
func trimPaths(set *orderedSet, prefix string) *orderedSet {
	if prefix == "" {
		return set
	}
	out := newOrderedSet()
	for _, path := range set.Items() {
		if strings.HasPrefix(path, prefix) {
			out.Add(path[len(prefix):])
		} else {
			out.Add(path)
		}
	}
	return out
}

// ExtractFiles is files.ts extractFiles(). Tool names are matched
// case-insensitively, mirroring the /i regexes in rank.go.
func ExtractFiles(blocks []NormalizedBlock, fileOps *FileOps) FileActivity {
	activity := FileActivity{Read: newOrderedSet(), Modified: newOrderedSet(), Created: newOrderedSet()}
	if fileOps != nil {
		for _, path := range fileOps.ReadFiles {
			activity.Read.Add(path)
		}
		for _, path := range fileOps.ModifiedFiles {
			activity.Modified.Add(path)
		}
		for _, path := range fileOps.CreatedFiles {
			activity.Created.Add(path)
		}
	}

	for _, block := range blocks {
		if block.Kind != KindToolCall {
			continue
		}
		name := strings.ToLower(block.Name)
		for _, path := range ExtractPaths(block.Args) {
			if fileReadTools[name] {
				activity.Read.Add(path)
			}
			if fileWriteTools[name] {
				activity.Modified.Add(path)
			}
			if fileCreateTools[name] {
				activity.Created.Add(path)
			}
		}
	}

	all := make([]string, 0, activity.Read.Len()+activity.Modified.Len()+activity.Created.Len())
	all = append(all, activity.Read.Items()...)
	all = append(all, activity.Modified.Items()...)
	all = append(all, activity.Created.Items()...)
	if prefix := longestCommonDirPrefix(all); prefix != "" {
		activity.Read = trimPaths(activity.Read, prefix)
		activity.Modified = trimPaths(activity.Modified, prefix)
		activity.Created = trimPaths(activity.Created, prefix)
	}
	return activity
}

// ── ordered string set ──────────────────────────────────────────────────────

// orderedSet is a JavaScript Set<string>: unique values in insertion order,
// with delete-then-add moving an entry to the tail. summarize.go's categorized
// merge relies on that order.
type orderedSet struct {
	items []string
	index map[string]int
}

func newOrderedSet() *orderedSet {
	return &orderedSet{index: make(map[string]int)}
}

// Add inserts a value, ignoring a value already present.
func (s *orderedSet) Add(value string) {
	if _, present := s.index[value]; present {
		return
	}
	s.index[value] = len(s.items)
	s.items = append(s.items, value)
}

// Has reports whether the set holds value.
func (s *orderedSet) Has(value string) bool {
	_, present := s.index[value]
	return present
}

// Delete removes a value, keeping the relative order of the rest.
func (s *orderedSet) Delete(value string) {
	position, present := s.index[value]
	if !present {
		return
	}
	s.items = append(s.items[:position:position], s.items[position+1:]...)
	delete(s.index, value)
	for i := position; i < len(s.items); i++ {
		s.index[s.items[i]] = i
	}
}

// Items returns the values in insertion order (a copy).
func (s *orderedSet) Items() []string {
	out := make([]string, len(s.items))
	copy(out, s.items)
	return out
}

// Len is the number of values.
func (s *orderedSet) Len() int { return len(s.items) }

// Clone copies the set.
func (s *orderedSet) Clone() *orderedSet {
	out := newOrderedSet()
	for _, item := range s.items {
		out.Add(item)
	}
	return out
}
