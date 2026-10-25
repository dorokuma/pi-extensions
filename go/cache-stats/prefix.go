package cachestats

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
)

// This file ports the read-only prefix-change diagnostics of cache-stats.ts
// (TS L879-1560): the fingerprint, the bounded object walk, the section
// extractors, the shape detector, the scope key, the LRU of per-scope
// snapshots, the status decision and the display helpers.
//
// Detection of side-effecting accessors and JS Proxy traps (isProxy /
// isUntrustedProxy, D83) has no Go equivalent: the SDK hands the handler plain
// decoded maps and slices, so a value read is always a data property. The
// walker keeps the budget, depth, node, cycle and finite-number guards, which
// are the parts that bound the work.

const (
	logName               = "cache-stats"
	piCacheStatsFooterEnv = "PI_CACHE_STATS_FOOTER"
	prefixDiagEnv         = "PI_CACHE_STATS_PREFIX_DIAGNOSTICS"
	turnReportLimit       = 50
	prefixScopeLimit      = 8
	prefixMaxDepth        = 8
	prefixMaxNodes        = 2000
	prefixMaxString       = 100_000
	prefixMaxTotalChars   = 250_000
	prefixMaxTools        = 64
	prefixNotifyChars     = 240
	prefixMaxScopeChars   = 16_384
)

// prefixCore is PREFIX_CORE: the sections whose fingerprints are compared.
var prefixCore = []string{"system", "developer", "instructions", "tools"}

type prefixSectionState string

const (
	statePresent prefixSectionState = "present"
	stateEmpty   prefixSectionState = "empty"
	stateMissing prefixSectionState = "missing"
	stateUnknown prefixSectionState = "unknown"
	stateSkipped prefixSectionState = "skipped"
)

type prefixShape string

const (
	shapeOpenAIChat      prefixShape = "openai-chat"
	shapeOpenAIResponses prefixShape = "openai-responses"
	shapeAnthropic       prefixShape = "anthropic"
	shapeUnknown         prefixShape = "unknown"
)

type prefixStatus string

const (
	statusBaseline prefixStatus = "baseline"
	statusStable   prefixStatus = "stable"
	statusChanged  prefixStatus = "changed"
	statusUnknown  prefixStatus = "unknown"
	statusSkipped  prefixStatus = "skipped"
	statusDisabled prefixStatus = "disabled"
)

// prefixSection is one fingerprinted section. A nil chars/count/fp is the TS
// undefined.
type prefixSection struct {
	state prefixSectionState
	chars *int
	count *int
	fp    *string
}

// prefixSections is the TS Record<string, PrefixSection>.
type prefixSections map[string]prefixSection

// prefixExtras is the extras pair of a view.
type prefixExtras struct {
	projectRules prefixSection
	skillsIndex  prefixSection
}

// storedPrefix is one cached per-scope snapshot.
type storedPrefix struct {
	comparable bool
	oversize   bool
	unsafe     bool
	shape      prefixShape
	sections   prefixSections
}

// prefixView is the last observation shown by /cache-stats prefix.
type prefixView struct {
	status   prefixStatus
	shape    prefixShape
	sections prefixSections
	extras   *prefixExtras
}

func newSection(state prefixSectionState) prefixSection {
	return prefixSection{state: state}
}

func (s prefixSection) withChars(chars int) prefixSection {
	s.chars = &chars
	return s
}

func (s prefixSection) withCount(count int) prefixSection {
	s.count = &count
	return s
}

func (s prefixSection) withFP(fp string) prefixSection {
	s.fp = &fp
	return s
}

func cloneSections(src prefixSections) prefixSections {
	out := make(prefixSections, len(src))
	for key, value := range src {
		out[key] = value
	}
	return out
}

// sectionsGet is `sections[key] ?? section("missing")`.
func sectionsGet(sections prefixSections, key string) prefixSection {
	if value, ok := sections[key]; ok {
		return value
	}
	return newSection(stateMissing)
}

func missingCoreSections() prefixSections {
	return prefixSections{
		"system":       newSection(stateMissing),
		"developer":    newSection(stateMissing),
		"instructions": newSection(stateMissing),
		"tools":        newSection(stateMissing),
	}
}

func skippedCoreSections() prefixSections {
	return prefixSections{
		"system":       newSection(stateSkipped),
		"developer":    newSection(stateSkipped),
		"instructions": newSection(stateSkipped),
		"tools":        newSection(stateSkipped),
	}
}

func unknownCoreSections() prefixSections {
	return prefixSections{
		"system":       newSection(stateUnknown),
		"developer":    newSection(stateUnknown),
		"instructions": newSection(stateUnknown),
		"tools":        newSection(stateUnknown),
	}
}

// comparableSection reports whether a section can take part in a comparison.
func comparableSection(s prefixSection) bool {
	return s.state == statePresent || s.state == stateEmpty
}

func fpEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// fingerprint hashes the salt and chunks with the TS 64-bit FNV-style mix. TS:
// fingerprint (L972-989).
//
// Both accumulators follow JavaScript's bitwise semantics exactly. The `>>> 0`
// after each imul keeps h1 and h2 unsigned, but the trailing `h2 ^=` is a
// plain XOR, which yields an int32. TS therefore prints h1 with toString(16)
// over a uint32 and h2 over a *signed* int32, so an h2 whose sign bit is set
// renders as "-xxxxxxxx" (nine characters) instead of "xxxxxxxx". The
// fixtures in prefix_test.go are cross-checked against the TS implementation,
// which is the only way to keep the two spellings byte-identical.
func fingerprint(salt [16]byte, chunks []string) string {
	h1 := uint32(2166136261)
	h2 := uint32(16777619)
	mix := func(c int64) {
		h1 = imul(h1^uint32(c&0xff), 16777619)
		h2 = imul(h2^uint32((c>>8)&0xff), 2166136261)
		h2 ^= imul(h1, 0x5bd1e995)
	}
	for _, b := range salt {
		mix(int64(b))
	}
	for _, chunk := range chunks {
		mix(31)
		units := utf16.Encode([]rune(chunk))
		length := int64(len(units))
		mix(length & 0xffff)
		mix(length >> 16)
		for _, unit := range units {
			mix(int64(unit))
		}
	}
	return padHex8(h1) + padInt32Hex8(h2)
}

// imul is JavaScript's Math.imul: a 32-bit wrapping signed multiply.
func imul(a, b uint32) uint32 {
	return uint32(int32(a) * int32(b))
}

// padHex8 renders an unsigned 32-bit value the way `h1.toString(16).padStart(8,
// "0")` prints it.
func padHex8(value uint32) string {
	text := strconv.FormatUint(uint64(value), 16)
	for len(text) < 8 {
		text = "0" + text
	}
	return text
}

// padInt32Hex8 renders a 32-bit accumulator the way `h2.toString(16).padStart(8,
// "0")` prints it. TS's h2 went through a plain `^`, so it is an int32 and its
// toString(16) carries a leading "-" whenever bit 31 is set. padStart pads the
// *whole* signed string to eight characters, so a short negative keeps its sign
// and gains the zeros after it: (-1) renders "000000-1" (8 chars), (-0x1234)
// renders "000-1234". Only magnitudes that already fill eight characters
// (e.g. -0x7fffffff -> "-7fffffff") come out with the ninth sign character.
// The vectors in prefix_test.go are cross-checked against Node.
func padInt32Hex8(value uint32) string {
	signed := strconv.FormatInt(int64(int32(value)), 16)
	if len(signed) < 8 {
		signed = strings.Repeat("0", 8-len(signed)) + signed
	}
	return signed
}

// escapeForNotify drops C0/C1/DEL/line separators and caps the length. It is
// display-only and never a scope or identity key. TS: escapeForNotify
// (L938-951).
func escapeForNotify(s string) string {
	var out strings.Builder
	units := 0
	for _, ch := range s {
		c := ch
		if c < 32 || c == 127 || (c >= 0x80 && c <= 0x9f) || c == 0x2028 || c == 0x2029 {
			continue
		}
		out.WriteRune(ch)
		units += utf16.RuneLen(ch)
		if units >= prefixNotifyChars {
			out.WriteString("…")
			break
		}
	}
	return out.String()
}

// prefixBudget bounds one walk. TS: PrefixBudget (L884).
type prefixBudget struct {
	nodes    int
	chars    int
	depth    int
	oversize bool
	unsafe   bool
}

func chargeChars(budget *prefixBudget, n float64) bool {
	if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		budget.unsafe = true
		return false
	}
	if n > prefixMaxString {
		budget.oversize = true
		return false
	}
	budget.chars += int(n)
	if budget.chars > prefixMaxTotalChars {
		budget.oversize = true
		return false
	}
	return true
}

func chargeString(budget *prefixBudget, s string) bool {
	return chargeChars(budget, float64(utf16Length(s)))
}

func bumpNodes(budget *prefixBudget) bool {
	budget.nodes++
	if budget.nodes > prefixMaxNodes {
		budget.oversize = true
		return false
	}
	return true
}

func markUnsafe(budget *prefixBudget) bool {
	budget.unsafe = true
	return false
}

func markOversize(budget *prefixBudget) bool {
	budget.oversize = true
	return false
}

// utf16Length is a JavaScript string's length.
func utf16Length(s string) int {
	return len(utf16.Encode([]rune(s)))
}

// hashValue fingerprints a value under the budget, or nil when the walk was
// incomplete or unsafe. TS: hashValue (L1032-1037).
func hashValue(salt [16]byte, value any, budget *prefixBudget, seen map[uintptr]bool) *string {
	acc := []string{}
	if !writeValue(value, &acc, budget, seen, 0) || budget.oversize || budget.unsafe {
		return nil
	}
	fp := fingerprint(salt, acc)
	return &fp
}

// writeValue appends the canonical encoding of value to acc. TS: writeValue
// (L1039-1104).
func writeValue(value any, acc *[]string, budget *prefixBudget, seen map[uintptr]bool, depth int) bool {
	if !bumpNodes(budget) || depth > prefixMaxDepth {
		return markOversize(budget)
	}
	if value == nil {
		*acc = append(*acc, "N")
		return true
	}
	switch typed := value.(type) {
	case bool:
		if typed {
			*acc = append(*acc, "B1")
		} else {
			*acc = append(*acc, "B0")
		}
		return true
	case string:
		if !chargeString(budget, typed) {
			return false
		}
		*acc = append(*acc, "S", strconv.Itoa(utf16Length(typed)), typed)
		return true
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return markUnsafe(budget)
		}
		n := jsNumberString(typed)
		if !chargeString(budget, n) {
			return false
		}
		*acc = append(*acc, "D", n)
		return true
	case int:
		n := jsNumberString(float64(typed))
		if !chargeString(budget, n) {
			return false
		}
		*acc = append(*acc, "D", n)
		return true
	case int64:
		n := jsNumberString(float64(typed))
		if !chargeString(budget, n) {
			return false
		}
		*acc = append(*acc, "D", n)
		return true
	case []any:
		if len(typed) > prefixMaxNodes {
			return markOversize(budget)
		}
		ptr := slicePointer(typed)
		if ptr != 0 {
			if seen[ptr] {
				return markUnsafe(budget)
			}
			seen[ptr] = true
			defer delete(seen, ptr)
		}
		*acc = append(*acc, "A", strconv.Itoa(len(typed)))
		for _, element := range typed {
			if !writeValue(element, acc, budget, seen, depth+1) {
				return false
			}
		}
		return true
	default:
		obj, ok := asObject(value)
		if !ok {
			return markUnsafe(budget)
		}
		keys := obj.keys()
		if len(keys) > prefixMaxNodes {
			return markOversize(budget)
		}
		ptr := objectPointer(value)
		if ptr != 0 {
			if seen[ptr] {
				return markUnsafe(budget)
			}
			seen[ptr] = true
			defer delete(seen, ptr)
		}
		*acc = append(*acc, "O", strconv.Itoa(len(keys)))
		for _, key := range keys {
			if !chargeString(budget, key) {
				return false
			}
			*acc = append(*acc, "K", key)
			element, present := obj.lookup(key)
			if !present {
				return markUnsafe(budget)
			}
			if !writeValue(element, acc, budget, seen, depth+1) {
				return false
			}
		}
		return true
	}
}

// slicePointer is the identity of a slice for cycle detection.
func slicePointer(values []any) uintptr {
	if len(values) == 0 {
		return 0
	}
	return pointerOf(values)
}

// objectPointer is the identity of a map for cycle detection.
func objectPointer(value any) uintptr {
	switch typed := value.(type) {
	case map[string]any:
		if typed == nil {
			return 0
		}
		return pointerOf(typed)
	}
	return 0
}

// textExtract is TextExtract. structChunks nil is the TS undefined struct.
type textExtract struct {
	kind         prefixSectionState // text/empty/unknown/skipped/missing
	text         string
	chars        int
	structChunks []string
}

// extractPlainText extracts the plain text of a message content value. TS:
// extractPlainText (L1106-1189).
func extractPlainText(content any, budget *prefixBudget) textExtract {
	if text, ok := content.(string); ok {
		if !chargeString(budget, text) {
			return textExtract{kind: stateSkipped}
		}
		structChunks := []string{"repr", "string", text}
		if utf16Length(text) == 0 {
			return textExtract{kind: stateEmpty, text: "", chars: 0, structChunks: structChunks}
		}
		return textExtract{kind: statePresent, text: text, chars: utf16Length(text), structChunks: structChunks}
	}
	if content == nil || !isObjectOrArray(content) {
		return textExtract{kind: stateUnknown}
	}
	array, ok := content.([]any)
	if !ok {
		return textExtract{kind: stateUnknown}
	}
	if len(array) > prefixMaxNodes {
		budget.oversize = true
		return textExtract{kind: stateSkipped}
	}
	parts := []string{}
	structChunks := []string{"repr", "array", "n", strconv.Itoa(len(array))}
	if !chargeString(budget, strconv.Itoa(len(array))) {
		return textExtract{kind: stateSkipped}
	}
	chars := 0
	for _, part := range array {
		if !bumpNodes(budget) {
			return textExtract{kind: stateSkipped}
		}
		if partText, ok := part.(string); ok {
			if !chargeString(budget, partText) {
				return textExtract{kind: stateSkipped}
			}
			parts = append(parts, partText)
			chars += utf16Length(partText)
			structChunks = append(structChunks, "item-string", partText)
			continue
		}
		block, ok := asObject(part)
		if !ok {
			return textExtract{kind: stateUnknown}
		}
		typv, _ := block.lookup("type")
		textv, _ := block.lookup("text")
		typeName, typeIsString := typv.(string)
		textValue, textIsString := textv.(string)
		if typeIsString && (typeName == "text" || typeName == "input_text" || typeName == "output_text") && textIsString {
			if !chargeString(budget, typeName) {
				return textExtract{kind: stateSkipped}
			}
			if !chargeString(budget, textValue) {
				return textExtract{kind: stateSkipped}
			}
			parts = append(parts, textValue)
			chars += utf16Length(textValue)
			structChunks = append(structChunks, "item-block", typeName, textValue)
			continue
		}
		return textExtract{kind: stateUnknown}
	}
	text := strings.Join(parts, "")
	if utf16Length(text) == 0 {
		return textExtract{kind: stateEmpty, text: "", chars: 0, structChunks: structChunks}
	}
	return textExtract{kind: statePresent, text: text, chars: chars, structChunks: structChunks}
}

// isObjectOrArray is `typeof value === "object" && value !== null`.
func isObjectOrArray(value any) bool {
	if value == nil {
		return false
	}
	if isObjectValue(value) {
		return true
	}
	return isArrayValue(value)
}

// textToSection maps a text extract to a section. TS: textToSection
// (L1191-1198).
func textToSection(salt [16]byte, ex textExtract) prefixSection {
	if ex.kind == stateSkipped {
		return newSection(stateSkipped)
	}
	if ex.kind == stateUnknown {
		return newSection(stateUnknown)
	}
	if ex.kind == stateMissing {
		return newSection(stateMissing)
	}
	chunks := ex.structChunks
	if chunks == nil {
		if ex.kind == stateEmpty {
			chunks = []string{"empty"}
		} else {
			chunks = []string{"text", ex.text}
		}
	}
	if ex.kind == stateEmpty {
		return newSection(stateEmpty).withChars(0).withFP(fingerprint(salt, chunks))
	}
	return newSection(statePresent).withChars(ex.chars).withFP(fingerprint(salt, chunks))
}

// roleCollect is RoleCollect: a role bag or the reason it could not be read.
type roleCollect struct {
	ok        bool
	kind      prefixSectionState // skipped/unknown when !ok
	system    textExtract
	developer textExtract
}

// collectRoleBag merges the system and developer messages of a chat/input
// array. TS: collectRoleBag (L1205-1281).
func collectRoleBag(list any, budget *prefixBudget) roleCollect {
	if list == nil {
		return roleCollect{ok: true, system: textExtract{kind: stateMissing}, developer: textExtract{kind: stateMissing}}
	}
	if !isObjectOrArray(list) {
		return roleCollect{ok: true, system: textExtract{kind: stateMissing}, developer: textExtract{kind: stateMissing}}
	}
	array, ok := list.([]any)
	if !ok {
		return roleCollect{ok: true, system: textExtract{kind: stateMissing}, developer: textExtract{kind: stateMissing}}
	}
	if len(array) > prefixMaxNodes {
		budget.oversize = true
		return roleCollect{ok: false, kind: stateSkipped}
	}
	type targetHit struct {
		index int
		ex    textExtract
	}
	found := map[string][]targetHit{"system": {}, "developer": {}}
	type layoutRow struct {
		index int
		role  string
	}
	layoutRows := []layoutRow{}
	for index, item := range array {
		if !bumpNodes(budget) {
			return roleCollect{ok: false, kind: stateSkipped}
		}
		message, ok := asObject(item)
		if !ok {
			continue
		}
		roleValue, present := message.lookup("role")
		if !present {
			continue
		}
		role, isString := roleValue.(string)
		if !isString {
			continue
		}
		if !chargeString(budget, role) {
			return roleCollect{ok: false, kind: stateSkipped}
		}
		if !chargeString(budget, strconv.Itoa(index)) {
			return roleCollect{ok: false, kind: stateSkipped}
		}
		layoutRows = append(layoutRows, layoutRow{index: index, role: role})
		if role != "system" && role != "developer" {
			continue
		}
		content, _ := message.lookup("content")
		ex := extractPlainText(content, budget)
		if ex.kind == stateSkipped || ex.kind == stateUnknown {
			return roleCollect{ok: false, kind: ex.kind}
		}
		found[role] = append(found[role], targetHit{index: index, ex: ex})
	}
	lastTarget := -1
	for _, role := range []string{"system", "developer"} {
		for _, hit := range found[role] {
			if hit.index > lastTarget {
				lastTarget = hit.index
			}
		}
	}
	layout := []string{"layout"}
	for _, row := range layoutRows {
		if row.index > lastTarget {
			break
		}
		layout = append(layout, "at", strconv.Itoa(row.index), row.role)
	}
	toExtract := func(role string) textExtract {
		hits := found[role]
		if len(hits) == 0 {
			return textExtract{kind: stateMissing}
		}
		structChunks := append([]string{"rolebag", role, "n", strconv.Itoa(len(hits))}, layout...)
		parts := []string{}
		for _, hit := range hits {
			structChunks = append(structChunks, "at", strconv.Itoa(hit.index))
			if hit.ex.structChunks != nil {
				structChunks = append(structChunks, hit.ex.structChunks...)
			}
			parts = append(parts, hit.ex.text)
		}
		text := strings.Join(parts, "\n")
		if utf16Length(text) == 0 {
			return textExtract{kind: stateEmpty, text: "", chars: 0, structChunks: structChunks}
		}
		return textExtract{kind: statePresent, text: text, chars: utf16Length(text), structChunks: structChunks}
	}
	return roleCollect{ok: true, system: toExtract("system"), developer: toExtract("developer")}
}

// detectShape classifies the provider payload structure. TS: detectShape
// (L1283-1311).
func detectShape(payload jsonObject) prefixShape {
	system, systemOwn := payload.lookup("system")
	_, instructionsOwn := payload.lookup("instructions")
	messages, messagesOwn := payload.lookup("messages")
	input, inputOwn := payload.lookup("input")

	systemAnthropic := systemOwn && (isStringValue(system) || isArrayValue(system))
	inputArr := inputOwn && isArrayValue(input)
	inputStr := inputOwn && isStringValue(input)
	messagesArr := messagesOwn && isArrayValue(messages)
	// Mixed exclusive fields: own-data existence, not truthiness.
	if messagesOwn && (inputOwn || instructionsOwn) {
		return shapeUnknown
	}
	if systemAnthropic && (instructionsOwn || inputOwn) {
		return shapeUnknown
	}
	if systemAnthropic {
		return shapeAnthropic
	}
	if instructionsOwn || inputArr || inputStr {
		return shapeOpenAIResponses
	}
	if messagesArr {
		return shapeOpenAIChat
	}
	return shapeUnknown
}

func isStringValue(value any) bool {
	_, ok := value.(string)
	return ok
}

// toolsSection fingerprints the tool array. TS: toolsSection (L1313-1330).
func toolsSection(salt [16]byte, own bool, value any, budget *prefixBudget) prefixSection {
	if !own {
		return newSection(stateMissing)
	}
	array, ok := value.([]any)
	if !ok {
		return newSection(stateUnknown)
	}
	if len(array) == 0 {
		return newSection(stateEmpty).withCount(0).withFP(fingerprint(salt, []string{"tools-empty"}))
	}
	if len(array) > prefixMaxTools {
		budget.oversize = true
		return newSection(stateSkipped)
	}
	fp := hashValue(salt, value, budget, map[uintptr]bool{})
	if fp == nil {
		if budget.oversize {
			return newSection(stateSkipped)
		}
		return newSection(stateUnknown)
	}
	return newSection(statePresent).withCount(len(array)).withFP(*fp)
}

// tagNameBoundary reports whether index starts a supported tag-name end. TS:
// tagNameBoundary (L1333-1337).
func tagNameBoundary(text string, index int) bool {
	if index >= len(text) {
		return false
	}
	c := text[index]
	return c == 62 || c == 32 || c == 9 || c == 10 || c == 13
}

// nextOpenTag finds the next supported open tag. TS: nextOpenTag (L1339-1349).
func nextOpenTag(text, openLiteral string, allowAttrs bool, from int) int {
	j := from
	for j < len(text) {
		p := strings.Index(text[j:], openLiteral)
		if p < 0 {
			return -1
		}
		p += j
		if !allowAttrs || tagNameBoundary(text, p+len(openLiteral)) {
			return p
		}
		j = p + len(openLiteral)
	}
	return -1
}

// scanUniqueTaggedBlock scans for exactly one complete tagged block. TS:
// scanUniqueTaggedBlock (L1351-1401).
func scanUniqueTaggedBlock(text, openLiteral, closeLiteral string, allowAttrs bool) (string, bool) {
	foundStart := -1
	foundEnd := -1
	i := 0
	n := len(text)
	for i < n {
		openAt := nextOpenTag(text, openLiteral, allowAttrs, i)
		closeBefore := strings.Index(text[i:], closeLiteral)
		if closeBefore >= 0 {
			closeBefore += i
		}
		if openAt < 0 {
			if closeBefore >= 0 {
				return "", false
			}
			break
		}
		if closeBefore >= 0 && closeBefore < openAt {
			return "", false
		}
		var contentStart int
		if allowAttrs {
			nameEnd := openAt + len(openLiteral)
			gt := strings.Index(text[nameEnd:], ">")
			if gt < 0 {
				return "", false
			}
			gt += nameEnd
			lt := strings.Index(text[nameEnd:], "<")
			if lt >= 0 && nameEnd+lt < gt {
				return "", false
			}
			contentStart = gt + 1
		} else {
			contentStart = openAt + len(openLiteral)
		}
		nestedOpen := nextOpenTag(text, openLiteral, allowAttrs, contentStart)
		closeAt := strings.Index(text[contentStart:], closeLiteral)
		if closeAt < 0 {
			return "", false
		}
		closeAt += contentStart
		if nestedOpen >= 0 && nestedOpen < closeAt {
			return "", false
		}
		if foundStart >= 0 {
			return "", false
		}
		foundStart = openAt
		foundEnd = closeAt + len(closeLiteral)
		i = foundEnd
	}
	if foundStart < 0 {
		return "", false
	}
	return text[foundStart:foundEnd], true
}

// extraFromSystemText extracts the project-instructions and skills-index
// blocks of the system text. TS: extraFromSystemText (L1403-1414).
func extraFromSystemText(salt [16]byte, text string) *prefixExtras {
	unidentified := newSection(stateUnknown)
	if utf16Length(text) == 0 {
		return &prefixExtras{projectRules: unidentified, skillsIndex: unidentified}
	}
	one := func(block string, ok bool) prefixSection {
		if !ok {
			return unidentified
		}
		return newSection(statePresent).withChars(utf16Length(block)).withFP(fingerprint(salt, []string{"extra", block}))
	}
	projectRules, projectOK := scanUniqueTaggedBlock(text, "<project_instructions", "</project_instructions>", true)
	skillsIndex, skillsOK := scanUniqueTaggedBlock(text, "<available_skills>", "</available_skills>", false)
	return &prefixExtras{
		projectRules: one(projectRules, projectOK),
		skillsIndex:  one(skillsIndex, skillsOK),
	}
}

// sectionFromExtract maps a role extract to a section and its text. TS:
// sectionFromExtract (L1416-1422).
func sectionFromExtract(salt [16]byte, ex textExtract) (prefixSection, *string) {
	if ex.kind == stateMissing {
		return newSection(stateMissing), nil
	}
	sec := textToSection(salt, ex)
	if ex.kind == statePresent {
		text := ex.text
		return sec, &text
	}
	if ex.kind == stateEmpty {
		text := ""
		return sec, &text
	}
	return sec, nil
}

// topTextSection reads one top-level text section. TS: topTextSection
// (L1424-1427).
func topTextSection(salt [16]byte, own bool, value any, budget *prefixBudget) (prefixSection, *string) {
	if !own {
		return newSection(stateMissing), nil
	}
	return sectionFromExtract(salt, extractPlainText(value, budget))
}

// boundedScopeString accepts a non-empty string within the scope budget. TS:
// boundedScopeString (L1429-1434).
func boundedScopeString(value *string) (string, bool) {
	if value == nil {
		return "", false
	}
	if utf16Length(*value) == 0 {
		return "", false
	}
	if utf16Length(*value) > prefixMaxScopeChars {
		return "", false
	}
	return *value, true
}

// scopeIdentity is the comparison scope of one observation. Each field is nil
// when the host did not provide it, which makes the scope key unavailable.
type scopeIdentity struct {
	provider *string
	api      *string
	id       *string
	baseURL  *string
	session  *string
}

// scopeKey fingerprints the comparison scope, or reports that it is
// unavailable. TS: scopeKey (L1443-1472) over readSessionToken and
// readEndpointToken.
func scopeKey(salt [16]byte, identity scopeIdentity) (string, bool) {
	provider, ok := boundedScopeString(identity.provider)
	if !ok {
		return "", false
	}
	api, ok := boundedScopeString(identity.api)
	if !ok {
		return "", false
	}
	id, ok := boundedScopeString(identity.id)
	if !ok {
		return "", false
	}
	session, ok := boundedScopeString(identity.session)
	if !ok {
		return "", false
	}
	baseURL, ok := boundedScopeString(identity.baseURL)
	if !ok {
		return "", false
	}
	endpointToken := "endpoint:" + fingerprint(salt, []string{"endpoint", baseURL})
	key := fingerprint(salt, []string{
		"scope",
		fingerprint(salt, []string{"session", session}),
		provider,
		api,
		id,
		fingerprint(salt, []string{endpointToken}),
	})
	return key, true
}

// lruMap is the TS Map used as a small LRU: insertion order is the recency
// order and the oldest entry is evicted past max.
type lruMap[V any] struct {
	keys   []string
	values map[string]V
	max    int
}

func newLRUMap[V any](max int) *lruMap[V] {
	return &lruMap[V]{values: map[string]V{}, max: max}
}

func (m *lruMap[V]) remove(key string) {
	for i, existing := range m.keys {
		if existing == key {
			m.keys = append(m.keys[:i], m.keys[i+1:]...)
			return
		}
	}
}

// set stores value under key as the most recent entry. TS: lruSet (L1474-1483).
func (m *lruMap[V]) set(key string, value V) {
	m.remove(key)
	m.keys = append(m.keys, key)
	m.values[key] = value
	for len(m.keys) > m.max {
		oldest := m.keys[0]
		m.keys = m.keys[1:]
		delete(m.values, oldest)
	}
}

// get returns the value of key and marks it the most recent. TS: lruGet
// (L1485-1492).
func (m *lruMap[V]) get(key string) (V, bool) {
	value, ok := m.values[key]
	if !ok {
		return value, false
	}
	m.remove(key)
	m.keys = append(m.keys, key)
	return value, true
}

func (m *lruMap[V]) clear() {
	m.keys = nil
	m.values = map[string]V{}
}

// decidePrefixStatus compares the current snapshot with the previous one. TS:
// decidePrefixStatus (L1498-1527).
func decidePrefixStatus(prev *storedPrefix, cur storedPrefix) prefixStatus {
	if cur.oversize || cur.unsafe {
		return statusSkipped
	}
	hasKnown := false
	for _, key := range prefixCore {
		if sectionsGet(cur.sections, key).state == stateSkipped {
			return statusSkipped
		}
		if comparableSection(sectionsGet(cur.sections, key)) {
			hasKnown = true
		}
	}
	if !hasKnown {
		return statusUnknown
	}
	if prev == nil || prev.oversize || prev.unsafe || !prev.comparable {
		return statusBaseline
	}
	if prev.shape != cur.shape {
		return statusChanged
	}
	compared := 0
	for _, key := range prefixCore {
		a := sectionsGet(prev.sections, key)
		b := sectionsGet(cur.sections, key)
		if a.state == stateSkipped || b.state == stateSkipped {
			return statusSkipped
		}
		if a.state == stateUnknown || b.state == stateUnknown {
			if comparableSection(a) || comparableSection(b) {
				return statusUnknown
			}
			continue
		}
		if a.state == stateMissing && b.state == stateMissing {
			continue
		}
		if a.state == stateMissing || b.state == stateMissing {
			return statusChanged
		}
		compared++
		if !fpEqual(a.fp, b.fp) {
			return statusChanged
		}
	}
	if compared > 0 {
		return statusStable
	}
	return statusUnknown
}

// payloadSummary is the outcome of summarizePayload.
type payloadSummary struct {
	shape    prefixShape
	sections prefixSections
	extras   *prefixExtras
	stored   storedPrefix
}

// skippedStored builds the skipped summary. TS: the skippedStored helper
// (L1543-1556).
func skippedStored(unsafe, oversize bool) payloadSummary {
	sections := missingCoreSections()
	return payloadSummary{
		shape:    shapeUnknown,
		sections: sections,
		extras:   nil,
		stored:   storedPrefix{comparable: false, oversize: oversize, unsafe: unsafe, shape: shapeUnknown, sections: cloneSections(sections)},
	}
}

// summarizePayload reads the payload's prefix sections. TS: summarizePayload
// (L1530-1633).
func summarizePayload(salt [16]byte, payload any, budget *prefixBudget) payloadSummary {
	if payload == nil || isArrayValue(payload) {
		sections := missingCoreSections()
		return payloadSummary{
			shape:    shapeUnknown,
			sections: sections,
			extras:   nil,
			stored:   storedPrefix{comparable: false, shape: shapeUnknown, sections: cloneSections(sections)},
		}
	}
	rec, ok := asObject(payload)
	if !ok {
		sections := missingCoreSections()
		return payloadSummary{
			shape:    shapeUnknown,
			sections: sections,
			extras:   nil,
			stored:   storedPrefix{comparable: false, shape: shapeUnknown, sections: cloneSections(sections)},
		}
	}
	shape := detectShape(rec)
	toolsVal, toolsOwn := rec.lookup("tools")
	systemVal, systemOwn := rec.lookup("system")
	instructionsVal, instructionsOwn := rec.lookup("instructions")
	messagesVal, _ := rec.lookup("messages")
	inputVal, _ := rec.lookup("input")
	if shape == shapeUnknown {
		sections := unknownCoreSections()
		return payloadSummary{
			shape:    shape,
			sections: sections,
			extras:   nil,
			stored:   storedPrefix{comparable: false, oversize: budget.oversize, unsafe: budget.unsafe, shape: shape, sections: cloneSections(sections)},
		}
	}
	sections := missingCoreSections()
	sections["tools"] = toolsSection(salt, toolsOwn, toolsVal, budget)
	var systemText *string
	applyRoleBag := func(bag roleCollect) {
		if !bag.ok {
			state := stateUnknown
			if bag.kind == stateSkipped {
				state = stateSkipped
			}
			sections["system"] = newSection(state)
			sections["developer"] = newSection(state)
			return
		}
		sysSection, sysText := sectionFromExtract(salt, bag.system)
		devSection, devText := sectionFromExtract(salt, bag.developer)
		sections["system"] = sysSection
		sections["developer"] = devSection
		if systemText == nil && sysText != nil {
			systemText = sysText
		}
		if systemText == nil && devText != nil {
			systemText = devText
		}
	}
	switch shape {
	case shapeAnthropic:
		section, text := topTextSection(salt, systemOwn, systemVal, budget)
		sections["system"] = section
		if text != nil {
			systemText = text
		}
	case shapeOpenAIChat:
		applyRoleBag(collectRoleBag(messagesVal, budget))
	case shapeOpenAIResponses:
		section, text := topTextSection(salt, instructionsOwn, instructionsVal, budget)
		sections["instructions"] = section
		if text != nil {
			systemText = text
		}
		applyRoleBag(collectRoleBag(inputVal, budget))
	default:
		sections["system"] = newSection(stateUnknown)
		sections["developer"] = newSection(stateUnknown)
		sections["instructions"] = newSection(stateUnknown)
		sections["tools"] = newSection(stateUnknown)
	}
	var extras *prefixExtras
	if systemText != nil {
		extras = extraFromSystemText(salt, *systemText)
	}
	comparable := false
	for _, key := range prefixCore {
		if comparableSection(sectionsGet(sections, key)) {
			comparable = true
			break
		}
	}
	stored := storedPrefix{
		comparable: comparable,
		oversize:   budget.oversize,
		unsafe:     budget.unsafe,
		shape:      shape,
		sections: prefixSections{
			"system":       sectionsGet(sections, "system"),
			"developer":    sectionsGet(sections, "developer"),
			"instructions": sectionsGet(sections, "instructions"),
			"tools":        sectionsGet(sections, "tools"),
		},
	}
	return payloadSummary{shape: shape, sections: sections, extras: extras, stored: stored}
}

// formatSection renders one section line. TS: formatSection (L1635-1650).
func formatSection(name string, s prefixSection) string {
	if s.state == statePresent {
		bits := []string{name + ": present"}
		if s.chars != nil {
			bits = append(bits, "chars="+strconv.Itoa(*s.chars))
		}
		if s.count != nil {
			bits = append(bits, "count="+strconv.Itoa(*s.count))
		}
		return strings.Join(bits, " ")
	}
	if s.state == stateEmpty {
		bits := []string{name + ": empty"}
		if s.count != nil {
			bits = append(bits, "count="+strconv.Itoa(*s.count))
		}
		if s.chars != nil {
			bits = append(bits, "chars="+strconv.Itoa(*s.chars))
		}
		return strings.Join(bits, " ")
	}
	return name + ": " + string(s.state)
}

// prefixStatsLine is the one-line prefix summary of /cache-stats. TS:
// prefixStatsLine (L1673-1680).
func prefixStatsLine(state *instanceState) string {
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.prefixDiagEnabled || !state.runtimeEnabled {
		return "Prefix: disabled"
	}
	if state.lastPrefixDiag == nil {
		return "Prefix: n/a (no request observed)"
	}
	return "Prefix: " + string(state.lastPrefixDiag.status) + " shape=" + string(state.lastPrefixDiag.shape)
}

// prefixLines renders /cache-stats prefix. TS: showPrefix (L1656-1671).
func prefixLines(state *instanceState) []string {
	state.mu.Lock()
	disabled := !state.runtimeEnabled || !state.prefixDiagEnabled
	last := state.lastPrefixDiag
	state.mu.Unlock()

	lines := []string{
		"Prefix diagnostics (this extension hook snapshot; not final HTTP body; not server cache prefix; chars ≠ tokens; not a hit-rate or savings claim)",
	}
	if disabled {
		lines = append(lines, "Status: disabled")
		return lines
	}
	if last == nil {
		lines = append(lines, "Status: n/a (no request observed)")
		return lines
	}
	lines = append(lines, "Status: "+string(last.status))
	lines = append(lines, "Shape: "+string(last.shape))
	for _, key := range prefixCore {
		lines = append(lines, formatSection(key, sectionsGet(last.sections, key)))
	}
	if last.extras != nil {
		pr := last.extras.projectRules
		sk := last.extras.skillsIndex
		if pr.state == statePresent {
			lines = append(lines, formatSection("project_rules", pr))
		} else {
			lines = append(lines, "project_rules: unidentified")
		}
		if sk.state == statePresent {
			lines = append(lines, formatSection("skills_index", sk))
		} else {
			lines = append(lines, "skills_index: unidentified")
		}
	}
	return lines
}

// notifyLines sends each line through ctx.ui.notify with the sanitizing
// escapeForNotify, as the TS command handlers do.
func notifyLines(call func(message, level string), lines []string) {
	for _, line := range lines {
		call(escapeForNotify(line), "info")
	}
}
