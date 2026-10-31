package herdsman

// agentContextMarker is the literal marker the TS extension filters out of the
// incoming context list. It is assembled at compile time from two halves so
// this repository can be read, grepped and diffed inside a Pi session that is
// itself running the TS herdsman-pi extension: that extension's `context` hook
// drops any message whose content contains the marker, and a tool result
// echoing it verbatim would be filtered away before the model could see it.
//
// The compiled string is byte-identical to the upstream literal (the same
// technique the port's tests use). Never inline the joined value at another
// call site.
const agentContextMarker = "[HERDSMAN " + "AGENT CONTEXT]"

// agentUpdatesHeading is the second half of a wake message: the policy block is
// followed by this heading and the formatted updates.
const agentUpdatesHeading = "[HERDSMAN AGENT UPDATES]"

// Custom types this extension reads and writes.
const (
	// wakeContextCustomType is the `customType` of the hidden wake context.
	wakeContextCustomType = "herdsman-wake-context"
	// herdsmanAgentContextCustomType is the legacy pinned-context type dropped
	// from the incoming context list.
	herdsmanAgentContextCustomType = "herdsman-agent-context"
	// herdsmanWakeQueuedCustomType is the older queued-wake type kept for
	// session replay defence.
	herdsmanWakeQueuedCustomType = "herdsman-wake-queued"
	// agentEventEntryType is the session entry type `appendEntry` writes.
	agentEventEntryType = "herdsman.agent_event"
)
