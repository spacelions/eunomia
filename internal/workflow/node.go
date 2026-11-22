package workflow

// NodeType discriminates the flat 1-level AST node kinds.
type NodeType string

const (
	NodeSequence      NodeType = "sequence"
	NodeParallel      NodeType = "parallel"
	NodeSwitch        NodeType = "switch"
	NodeAction        NodeType = "action"
	NodeInteraction   NodeType = "interaction"
	NodeAgent         NodeType = "agent"
	NodeChildWorkflow NodeType = "child_workflow"
)

// Node is a single block or leaf step in the workflow AST.
// Exactly one typed spec pointer must be non-nil and match Type.
type Node struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	Type          NodeType           `json:"type"`
	Sequence      *SequenceSpec      `json:"sequence,omitempty"`
	Parallel      *ParallelSpec      `json:"parallel,omitempty"`
	Switch        *SwitchSpec        `json:"switch,omitempty"`
	Action        *ActionSpec        `json:"action,omitempty"`
	Interaction   *InteractionSpec   `json:"interaction,omitempty"`
	Agent         *AgentSpec         `json:"agent,omitempty"`
	ChildWorkflow *ChildWorkflowSpec `json:"child_workflow,omitempty"`
}

// SequenceSpec executes Steps strictly in order.
type SequenceSpec struct {
	Steps []Node `json:"steps"`
}

// ParallelSpec executes Branches concurrently and joins when all complete.
type ParallelSpec struct {
	Branches []Node `json:"branches"`
}

// SwitchSpec evaluates Cases in order using expr-lang/expr conditions, falling back to Default.
type SwitchSpec struct {
	Cases   []SwitchCase `json:"cases"`
	Default *Node        `json:"default,omitempty"`
}

// SwitchCase pairs an expr-lang boolean condition with its target AST node.
type SwitchCase struct {
	When string `json:"when"`
	Node Node   `json:"node"`
}

// ActionSpec executes a deterministic, idempotent Connector call keyed by runID:stepID.
// When Archetype is "observer", the step represents a non-blocking push (e.g., Notion/Gmail).
// When Operation is "http", HTTP holds a build-time AI-compiled declarative HTTP call.
type ActionSpec struct {
	Archetype Archetype            `json:"archetype,omitempty"`
	Connector string               `json:"connector"`
	Operation string               `json:"operation"`
	Recipient RoleBinding          `json:"recipient,omitempty"`
	Resources map[string]string    `json:"resources,omitempty"`
	Inputs    map[string]ValueExpr `json:"inputs,omitempty"`
	Outputs   map[string]string    `json:"outputs,omitempty"`
	HTTP      *HTTPActionSpec      `json:"http,omitempty"`
}

// HTTPActionSpec defines a build-time compiled deterministic HTTP request against a
// tenant connector's pinned base_url (SSRF-safe: relative Path only, credentials injected at runtime).
type HTTPActionSpec struct {
	Method             string               `json:"method"`
	Path               string               `json:"path"`
	Headers            map[string]ValueExpr `json:"headers,omitempty"`
	Query              map[string]ValueExpr `json:"query,omitempty"`
	Body               map[string]ValueExpr `json:"body,omitempty"`
	IdempotencyHeader  string               `json:"idempotency_header,omitempty"`
	SuccessStatuses    []int                `json:"success_statuses,omitempty"`
	IdempotentStatuses []int                `json:"idempotent_statuses,omitempty"`
	RetryableStatuses  []int                `json:"retryable_statuses,omitempty"`
	Extract            map[string]string    `json:"extract,omitempty"`
}

// InteractionSpec defines a blocking human step dispatched via a Channel (Magic Link / Slack card).
type InteractionSpec struct {
	Archetype     Archetype   `json:"archetype"`
	Channel       string      `json:"channel"`
	Assignee      RoleBinding `json:"assignee"`
	AllowDelegate *bool       `json:"allow_delegate,omitempty"`
	Prompt        *ValueExpr  `json:"prompt,omitempty"`
	Fields        []FormField `json:"fields,omitempty"`
	ReceiptFields []FormField `json:"receipt_fields,omitempty"`
	DraftGate     bool        `json:"draft_gate,omitempty"`
	OnReject      *Node       `json:"on_reject,omitempty"`
	SLA           *SLAConfig  `json:"sla,omitempty"`
	EscalateTo    RoleBinding `json:"escalate_to,omitempty"`
}

// FormField binds a user-submitted form input to a Case State key with optional live validators.
type FormField struct {
	StateKey   string         `json:"state_key"`
	Label      string         `json:"label"`
	Required   bool           `json:"required"`
	Validators []ValidatorRef `json:"validators,omitempty"`
}

// ValidatorRef points to a shift-left Connector validator invoked before form submission.
type ValidatorRef struct {
	Connector string               `json:"connector"`
	Validator string               `json:"validator"`
	Params    map[string]ValueExpr `json:"params,omitempty"`
}

// SLAConfig configures Temporal reminder and escalation timers (Go duration strings, e.g. "24h").
type SLAConfig struct {
	ReminderAfter string `json:"reminder_after,omitempty"`
	EscalateAfter string `json:"escalate_after,omitempty"`
}

// AgentSpec defines a bounded v0.2+ ADK execution inside a Temporal Activity.
type AgentSpec struct {
	Goal         ValueExpr         `json:"goal"`
	MCPTools     []string          `json:"mcp_tools,omitempty"`
	MaxTurns     int               `json:"max_turns"`
	OutputSchema map[string]any    `json:"output_schema,omitempty"`
	Outputs      map[string]string `json:"outputs,omitempty"`
}

// ChildWorkflowSpec invokes another published workflow version via ExecuteChildWorkflow (v0.2+).
type ChildWorkflowSpec struct {
	WorkflowID string               `json:"workflow_id"`
	Inputs     map[string]ValueExpr `json:"inputs,omitempty"`
	Outputs    map[string]string    `json:"outputs,omitempty"`
}
