package pivcc

import (
	"strings"
)

// Ports src/core/load-messages.ts and src/core/lineage.ts.

// LoadedMessages is load-messages.ts LoadedMessages.
type LoadedMessages struct {
	Rendered    []RenderedEntry
	RawMessages []map[string]any
}

// LoadAllMessages is load-messages.ts loadAllMessages(): every counted message
// entry of the session file, rendered and raw, numbered by the shared global
// index rule.
//
// allowedEntryIDs, when non-nil, keeps only entries it holds. role:"system"
// entries still occupy an index but are never rendered or searched.
func LoadAllMessages(sessionFile string, full bool, allowedEntryIDs map[string]bool) LoadedMessages {
	loaded := LoadedMessages{}
	messageIndex := 0
	_, _ = ForEachJSONLLine(sessionFile, func(line []byte) {
		if len(line) == 0 {
			return
		}
		entry, ok := decodeJSONObject(line)
		if !ok {
			return
		}
		// Counting rule shared with globalindices.go — both index spaces must
		// agree by construction.
		if !IsCountedMessageEntry(entry) {
			return
		}
		message, _ := entry["message"].(map[string]any)
		id, _ := entry["id"].(string)
		allowed := (allowedEntryIDs == nil || allowedEntryIDs[id]) && messageRole(message) != "system"
		if allowed {
			loaded.Rendered = append(loaded.Rendered, RenderMessage(message, messageIndex, full))
			loaded.RawMessages = append(loaded.RawMessages, message)
		}
		messageIndex++
	})
	return loaded
}

// LineageEntryLike is lineage.ts LineageEntryLike: the entry shape the lineage
// helpers need.
type LineageEntryLike map[string]any

// LineageSessionManager is lineage.ts LineageSessionManagerLike.
type LineageSessionManager interface {
	// GetBranch is the active conversation path, oldest first.
	GetBranch() ([]LineageEntryLike, error)
	// GetEntries is every entry of the session, in file order.
	GetEntries() ([]LineageEntryLike, error)
}

// GetActiveLineageEntryIDs is lineage.ts getActiveLineageEntryIds(): the ids on
// the active path, falling back to every entry's id, then to empty.
func GetActiveLineageEntryIDs(manager LineageSessionManager) map[string]bool {
	if manager != nil {
		if branch, err := manager.GetBranch(); err == nil && len(branch) > 0 {
			out := make(map[string]bool, len(branch))
			for _, entry := range branch {
				if id, ok := entry["id"].(string); ok && id != "" {
					out[id] = true
				}
			}
			return out
		}
		if all, err := manager.GetEntries(); err == nil {
			out := make(map[string]bool, len(all))
			for _, entry := range all {
				if id, ok := entry["id"].(string); ok && id != "" {
					out[id] = true
				}
			}
			return out
		}
	}
	return map[string]bool{}
}

var _ = strings.Join
