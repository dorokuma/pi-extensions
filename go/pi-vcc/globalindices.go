package pivcc

// Ports src/core/global-indices.ts: the single definition of the `#N` index
// space shared by the summary refs and recall.

// IsCountedMessageEntry is global-indices.ts isCountedMessageEntry(): the
// entries the global `#N` index counts.
func IsCountedMessageEntry(entry map[string]any) bool {
	if entry == nil {
		return false
	}
	kind, _ := entry["type"].(string)
	return kind == "message" && entry["message"] != nil
}

// globalIndexBuilder accumulates the id -> global-index mapping one entry at a
// time so the in-memory array path and the streaming file path share one
// counting rule.
type globalIndexBuilder struct {
	byID         map[string]int
	ambiguous    map[string]bool
	messageIndex int
}

func newGlobalIndexBuilder() *globalIndexBuilder {
	return &globalIndexBuilder{byID: make(map[string]int), ambiguous: make(map[string]bool)}
}

// add counts one entry. Entries without a usable id still occupy an index but
// produce no map entry; duplicate ids are dropped fail-closed.
func (b *globalIndexBuilder) add(entry map[string]any) {
	if !IsCountedMessageEntry(entry) {
		return
	}
	if id, ok := entry["id"].(string); ok && id != "" {
		if _, present := b.byID[id]; present || b.ambiguous[id] {
			delete(b.byID, id)
			b.ambiguous[id] = true
		} else {
			b.byID[id] = b.messageIndex
		}
	}
	b.messageIndex++
}

func (b *globalIndexBuilder) result() map[string]int {
	out := make(map[string]int, len(b.byID))
	for id, index := range b.byID {
		out[id] = index
	}
	return out
}

// BuildGlobalIndexByID is global-indices.ts buildGlobalIndexById(): the entry
// id -> global message index map, in array order.
func BuildGlobalIndexByID(entries []map[string]any) map[string]int {
	builder := newGlobalIndexBuilder()
	for _, entry := range entries {
		builder.add(entry)
	}
	return builder.result()
}

// LoadGlobalIndexByID is global-indices.ts loadGlobalIndexById(): the same map
// built by streaming a session JSONL file. Malformed lines are skipped; nil
// reports a file that cannot be read.
func LoadGlobalIndexByID(sessionFile string) map[string]int {
	builder := newGlobalIndexBuilder()
	found, err := ForEachJSONLLine(sessionFile, func(line []byte) {
		if len(line) == 0 {
			return
		}
		entry, ok := decodeJSONObject(line)
		if !ok {
			// Corrupt lines are silently dropped by pi too.
			return
		}
		builder.add(entry)
	})
	if err != nil || !found {
		return nil
	}
	return builder.result()
}
