package playbook

// SchemaVersionV1 is the canonical Markdown Playbook frontmatter schema version.
const SchemaVersionV1 = "v1"

// Archetype classifies abstract participant roles without hardcoding emails or channel IDs.
type Archetype string

const (
	ArchetypeRequestor           Archetype = "requestor"
	ArchetypeGroundTruthProvider Archetype = "ground_truth_provider"
	ArchetypeApprover            Archetype = "approver"
	ArchetypeFulfiller           Archetype = "fulfiller"
	ArchetypeEscalationOwner     Archetype = "escalation_owner"
	ArchetypeObserver            Archetype = "observer"
)

// Valid reports whether a is one of the six defined participant archetypes.
func (a Archetype) Valid() bool {
	switch a {
	case ArchetypeRequestor,
		ArchetypeGroundTruthProvider,
		ArchetypeApprover,
		ArchetypeFulfiller,
		ArchetypeEscalationOwner,
		ArchetypeObserver:
		return true
	default:
		return false
	}
}

// FieldType constrains typed fields on the shared Case State blackboard.
type FieldType string

const (
	FieldTypeString     FieldType = "string"
	FieldTypeEmail      FieldType = "email"
	FieldTypeInt        FieldType = "int"
	FieldTypeFloat      FieldType = "float"
	FieldTypeBool       FieldType = "bool"
	FieldTypeDate       FieldType = "date"
	FieldTypeStringList FieldType = "string_list"
	FieldTypeEnum       FieldType = "enum"
	FieldTypeFile       FieldType = "file"
	FieldTypeObject     FieldType = "object"
)

// Valid reports whether ft is a supported Case State field type.
func (ft FieldType) Valid() bool {
	switch ft {
	case FieldTypeString,
		FieldTypeEmail,
		FieldTypeInt,
		FieldTypeFloat,
		FieldTypeBool,
		FieldTypeDate,
		FieldTypeStringList,
		FieldTypeEnum,
		FieldTypeFile,
		FieldTypeObject:
		return true
	default:
		return false
	}
}

// FieldSpec describes a single typed entry on the trigger input or Case State blackboard.
type FieldSpec struct {
	Type        FieldType `yaml:"type" json:"type"`
	Options     []string  `yaml:"options,omitempty" json:"options,omitempty"`
	Description string    `yaml:"description,omitempty" json:"description,omitempty"`
	Sensitive   bool      `yaml:"sensitive,omitempty" json:"sensitive,omitempty"`
}

// StateSchema maps Case State keys to their typed definitions.
type StateSchema map[string]FieldSpec

// TriggerSpec declares how a playbook run is launched and what typed inputs the requestor supplies.
type TriggerSpec struct {
	Archetype Archetype   `yaml:"archetype" json:"archetype"`
	Inputs    StateSchema `yaml:"inputs" json:"inputs"`
}

// RoleSpec binds a logical playbook role name to an archetype and dynamic RoleBinding.
type RoleSpec struct {
	Archetype   Archetype   `yaml:"archetype" json:"archetype"`
	Binding     RoleBinding `yaml:"binding" json:"binding"`
	Description string      `yaml:"description,omitempty" json:"description,omitempty"`
}

// ConnectorAllowSpec whitelists the exact connector operations, semantic resource aliases,
// and optional build-time compiled HTTP operations available to the agent at runtime.
type ConnectorAllowSpec struct {
	Name           string                       `yaml:"name" json:"name"`
	Operations     []string                     `yaml:"operations,omitempty" json:"operations,omitempty"`
	Resources      []string                     `yaml:"resources,omitempty" json:"resources,omitempty"`
	HTTPOperations map[string]HTTPOperationSpec `yaml:"http_operations,omitempty" json:"http_operations,omitempty"`
}

// HTTPOperationSpec defines a build-time AI-compiled deterministic HTTP template against
// a tenant connector's pinned base_url (SSRF-safe: relative Path only; auth injected at runtime).
type HTTPOperationSpec struct {
	Method             string            `yaml:"method" json:"method"`
	Path               string            `yaml:"path" json:"path"`
	Description        string            `yaml:"description,omitempty" json:"description,omitempty"`
	IdempotencyHeader  string            `yaml:"idempotency_header,omitempty" json:"idempotency_header,omitempty"`
	SuccessStatuses    []int             `yaml:"success_statuses,omitempty" json:"success_statuses,omitempty"`
	IdempotentStatuses []int             `yaml:"idempotent_statuses,omitempty" json:"idempotent_statuses,omitempty"`
	RetryableStatuses  []int             `yaml:"retryable_statuses,omitempty" json:"retryable_statuses,omitempty"`
	Extract            map[string]string `yaml:"extract,omitempty" json:"extract,omitempty"`
}

// LimitsSpec sets hard deterministic bounds on agent execution inside Temporal.
type LimitsSpec struct {
	MaxTurns    int    `yaml:"max_turns" json:"max_turns"`
	MaxDuration string `yaml:"max_duration" json:"max_duration"`
}

// RequiredOutcomesSpec defines deterministic completion checks enforced by Go before
// the Temporal agent is allowed to finalize a run via complete_run.
type RequiredOutcomesSpec struct {
	StateKeys  []string `yaml:"state_keys,omitempty" json:"state_keys,omitempty"`
	Operations []string `yaml:"operations,omitempty" json:"operations,omitempty"`
}

// Frontmatter holds the strict, machine-validated rails declared at the top of a Playbook .md file.
type Frontmatter struct {
	SchemaVersion    string                `yaml:"schema_version" json:"schema_version"`
	PlaybookID       string                `yaml:"playbook_id" json:"playbook_id"`
	Version          int                   `yaml:"version" json:"version"`
	Name             string                `yaml:"name" json:"name"`
	Description      string                `yaml:"description,omitempty" json:"description,omitempty"`
	Owner            RoleBinding           `yaml:"owner" json:"owner"`
	Trigger          TriggerSpec           `yaml:"trigger" json:"trigger"`
	State            StateSchema           `yaml:"state,omitempty" json:"state,omitempty"`
	Roles            map[string]RoleSpec   `yaml:"roles,omitempty" json:"roles,omitempty"`
	Connectors       []ConnectorAllowSpec  `yaml:"connectors,omitempty" json:"connectors,omitempty"`
	Limits           LimitsSpec            `yaml:"limits" json:"limits"`
	RequiredOutcomes *RequiredOutcomesSpec `yaml:"required_outcomes,omitempty" json:"required_outcomes,omitempty"`
}

// Playbook is an immutable versioned Markdown Playbook combining strict deterministic
// YAML frontmatter rails with a human-authored Markdown SOP executed by a Temporal-backed ADK agent.
type Playbook struct {
	Frontmatter `yaml:",inline" json:"frontmatter"`
	Body        string `yaml:"-" json:"body"`
}
