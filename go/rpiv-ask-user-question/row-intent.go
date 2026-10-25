package rpivaskuserquestion

// Ports state/row-intent.ts (v2.15.0): the per-kind META table that every
// behavior-bearing branch reads.

// RowKind is the row discriminator. option rows are author-defined; other and
// next are the synthesized sentinels.
type RowKind string

// The three row kinds.
const (
	RowKindOption RowKind = "option"
	RowKindOther  RowKind = "other"
	RowKindNext   RowKind = "next"
)

// SentinelKind is the subset of RowKind representing protocol-driven rows.
type SentinelKind string

// The two sentinel kinds.
const (
	SentinelOther SentinelKind = "other"
	SentinelNext  SentinelKind = "next"
)

// SentinelKinds lists the sentinels in declaration order — the append walker,
// the reserved-label derivation and LABELS_BY_KIND consumers iterate it.
var SentinelKinds = []SentinelKind{SentinelOther, SentinelNext}

// RowIntentMeta is the per-kind static metadata. Pure data: no closures, no
// per-kind handler functions.
type RowIntentMeta struct {
	// Label is the user-facing string. For option it is an empty placeholder
	// (per-instance labels come from QuestionData.Options[i].Label).
	Label string
	// Reserved marks author-facing labels that trigger `reserved_label`.
	Reserved bool
	// LivesInMainList is true iff the row appears in itemsByTab.
	LivesInMainList bool
	// Numbered is true iff the row contributes to the main-list numbering.
	Numbered bool
	// ActivatesInputMode is true iff focusing the row toggles state.InputMode.
	ActivatesInputMode bool
	// BlocksMultiToggle suppresses Space/Enter-as-toggle in multiSelect mode.
	BlocksMultiToggle bool
	// AutoSubmitsInMulti commits the question on Enter in multiSelect mode.
	AutoSubmitsInMulti bool
	// AutoAppendOnSingleSelect appends this row for single-select questions.
	AutoAppendOnSingleSelect bool
	// AutoAppendOnMultiSelect appends this row for multi-select questions.
	AutoAppendOnMultiSelect bool
}

// rowIntentMeta is the META table, mirroring ROW_INTENT_META field for field.
var rowIntentMeta = map[SentinelKind]RowIntentMeta{
	SentinelOther: {
		Label:                    SentinelOtherLabel,
		Reserved:                 true,
		LivesInMainList:          true,
		Numbered:                 true,
		ActivatesInputMode:       true,
		BlocksMultiToggle:        false,
		AutoSubmitsInMulti:       false,
		AutoAppendOnSingleSelect: true,
		AutoAppendOnMultiSelect:  true,
	},
	SentinelNext: {
		Label:                    SentinelNextLabel,
		Reserved:                 true,
		LivesInMainList:          true,
		Numbered:                 false,
		ActivatesInputMode:       false,
		BlocksMultiToggle:        true,
		AutoSubmitsInMulti:       true,
		AutoAppendOnSingleSelect: false,
		AutoAppendOnMultiSelect:  true,
	},
}

// optionRowMeta is ROW_INTENT_META.option — every behavior flag false, the
// label empty (per-instance labels come from the question).
var optionRowMeta = RowIntentMeta{
	Label:                    "",
	Reserved:                 false,
	LivesInMainList:          true,
	Numbered:                 true,
	ActivatesInputMode:       false,
	BlocksMultiToggle:        false,
	AutoSubmitsInMulti:       false,
	AutoAppendOnSingleSelect: false,
	AutoAppendOnMultiSelect:  false,
}

// RowIntentMetaFor returns the META entry for a row kind.
func RowIntentMetaFor(kind RowKind) RowIntentMeta {
	if kind == RowKindOption {
		return optionRowMeta
	}
	return rowIntentMeta[SentinelKind(kind)]
}

// labelsByKind is the kind-keyed label view (`option` excluded: its label is
// per-instance, not per-kind).
var labelsByKind = map[SentinelKind]string{
	SentinelOther: SentinelOtherLabel,
	SentinelNext:  SentinelNextLabel,
}

// SentinelsToAppend mirrors sentinelsToAppend: walk the META table to
// synthesize sentinel rows for one question, in declaration order.
func SentinelsToAppend(question QuestionData) []SentinelKind {
	out := make([]SentinelKind, 0, len(SentinelKinds))
	for _, kind := range SentinelKinds {
		meta := rowIntentMeta[kind]
		if !meta.LivesInMainList {
			continue
		}
		if question.MultiSelect {
			if meta.AutoAppendOnMultiSelect {
				out = append(out, kind)
			}
			continue
		}
		if meta.AutoAppendOnSingleSelect {
			out = append(out, kind)
		}
	}
	return out
}

// RowActionForIndex mirrors the row-intent helper the TS source exports
// alongside the META table: the row kind at index, and the index itself.
func RowActionForIndex(items []WrappingSelectItem, index int) (WrappingSelectItem, bool) {
	if index < 0 || index >= len(items) {
		return WrappingSelectItem{}, false
	}
	return items[index], true
}
