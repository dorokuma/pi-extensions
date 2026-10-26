package herdsmanpi

// AgentEventWireRecord mirrors the TS `AgentEventWireRecord`: one row of the
// daemon's pending event queue. Pointer fields keep the TS `?:` and `| null`
// distinction, because the projection tests them for presence (a present empty
// string and an absent field are different).
type AgentEventWireRecord struct {
	AgentID          *string              `json:"agentId,omitempty"`
	Attempts         *int                 `json:"attempts,omitempty"`
	LastAttemptAt    *int64               `json:"lastAttemptAt,omitempty"`
	NextAttemptAt    *int64               `json:"nextAttemptAt,omitempty"`
	LastFailureCode  *string              `json:"lastFailureCode,omitempty"`
	CompactHistory   *CompactAgentHistory `json:"compactHistory,omitempty"`
	CreatedAt        *string              `json:"createdAt,omitempty"`
	HerdrSessionName *string              `json:"herdrSessionName,omitempty"`
	ID               int                  `json:"id"`
	PaneGeneration   *string              `json:"paneGeneration,omitempty"`
	PaneID           *string              `json:"paneId,omitempty"`
	Payload          any                  `json:"payload"`
	Status           *string              `json:"status,omitempty"`
	TerminalID       *string              `json:"terminalId,omitempty"`
	Type             string               `json:"type"`
	WorkspaceID      *string              `json:"workspaceId,omitempty"`
}

// CompactAgentHistory mirrors the TS `CompactAgentHistory`.
type CompactAgentHistory struct {
	LastAssistantMessage *CompactText `json:"lastAssistantMessage,omitempty"`
	LastToolResult       *CompactTool `json:"lastToolResult,omitempty"`
	LastUserMessage      *CompactText `json:"lastUserMessage,omitempty"`
	UpdatedAt            *string      `json:"updatedAt,omitempty"`
}

// CompactText is `{ text?: string | null }`.
type CompactText struct {
	Text *string `json:"text,omitempty"`
}

// CompactTool is `{ text?: string; toolName?: string }`.
type CompactTool struct {
	Text     *string `json:"text,omitempty"`
	ToolName *string `json:"toolName,omitempty"`
}

// AgentContextListItem mirrors the TS `AgentContextListItem`.
type AgentContextListItem struct {
	Agent       *string              `json:"agent,omitempty"`
	AgentStatus *string              `json:"agentStatus,omitempty"`
	History     *CompactAgentHistory `json:"history,omitempty"`
	ID          *string              `json:"id,omitempty"`
	Name        *string              `json:"name,omitempty"`
	PaneID      *string              `json:"paneId,omitempty"`
	TerminalID  *string              `json:"terminalId,omitempty"`
}

// AgentWorkspaceContextSnapshot mirrors the TS `AgentWorkspaceContextSnapshot`.
type AgentWorkspaceContextSnapshot struct {
	Agents           []AgentContextListItem `json:"agents"`
	HerdrSessionName string                 `json:"herdrSessionName"`
	UpdatedAt        string                 `json:"updatedAt"`
	WorkspaceID      string                 `json:"workspaceId"`
}

// AgentOrchestratorOwner mirrors the TS `AgentOrchestratorOwner`.
type AgentOrchestratorOwner struct {
	PaneID     string `json:"paneId"`
	TerminalID string `json:"terminalId"`
}

// AgentOrchestratorWireState mirrors the TS `AgentOrchestratorWireState`.
type AgentOrchestratorWireState struct {
	AckedEventID     int                     `json:"ackedEventId"`
	HerdrSessionName string                  `json:"herdrSessionName"`
	Owner            *AgentOrchestratorOwner `json:"owner"`
	UpdatedAt        string                  `json:"updatedAt"`
	WorkspaceID      string                  `json:"workspaceId"`
}

// AgentOrchestratorChanged mirrors the TS `AgentOrchestratorChanged`.
type AgentOrchestratorChanged struct {
	Current  AgentOrchestratorWireState `json:"current"`
	Previous AgentOrchestratorWireState `json:"previous"`
	Reason   string                     `json:"reason"`
}

// PiPresence mirrors the TS `PiPresence`.
type PiPresence struct {
	ConnectedAt      int64  `json:"connectedAt"`
	HerdrSessionName string `json:"herdrSessionName"`
	PaneID           string `json:"paneId"`
	SubscriberID     string `json:"subscriberId"`
	TerminalID       string `json:"terminalId"`
	WorkspaceID      string `json:"workspaceId"`
}

// ConnectionStateResponse mirrors the TS `ConnectionStateResponse`. The
// `ackedEventId`/`events`/`context`/`changed` fields are optional: an old
// daemon may omit them, which the TS reads as undefined.
type ConnectionStateResponse struct {
	Changed      *bool                          `json:"changed,omitempty"`
	AckedEventID *int                           `json:"ackedEventId,omitempty"`
	Context      *AgentWorkspaceContextSnapshot `json:"context,omitempty"`
	Events       []AgentEventWireRecord         `json:"events,omitempty"`
	Presence     PiPresence                     `json:"presence"`
	State        *AgentOrchestratorWireState    `json:"state"`
}

// DaemonStreamMessage mirrors the TS `DaemonStreamMessage` union: a stream
// notification carries a `method` and a `params` object whose shape depends on
// that method, so it is decoded lazily.
type DaemonStreamMessage struct {
	Method       string
	Event        *AgentEventWireRecord
	Context      *AgentWorkspaceContextSnapshot
	HerdrSession string
	WorkspaceID  string
	Change       *AgentOrchestratorChanged
}

// RpcMessage mirrors the TS `RpcMessage`.
type RpcMessage struct {
	Error  *RpcError `json:"error,omitempty"`
	ID     any       `json:"id,omitempty"`
	Method string    `json:"method,omitempty"`
	Params any       `json:"params,omitempty"`
	Result any       `json:"result,omitempty"`
}

// RpcError mirrors the TS `{ code?: string; message?: string }`.
type RpcError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}
