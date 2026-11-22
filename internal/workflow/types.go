package workflow

// SchemaVersionV1 is the current canonical JSON AST schema version.
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

// FieldSpec describes a single typed entry on the shared Case State blackboard.
type FieldSpec struct {
	Type        FieldType `json:"type"`
	Options     []string  `json:"options,omitempty"`
	Description string    `json:"description,omitempty"`
	Sensitive   bool      `json:"sensitive,omitempty"`
}

// StateSchema maps Case State keys to their typed definitions.
type StateSchema map[string]FieldSpec

// TriggerSpec declares how a workflow run is launched and which State keys the requestor supplies.
type TriggerSpec struct {
	Archetype Archetype `json:"archetype"`
	InputKeys []string  `json:"input_keys"`
}

// WorkflowDef is the canonical immutable workflow definition stored in Postgres JSONB
// and interpreted deterministically by Temporal.
type WorkflowDef struct {
	SchemaVersion string      `json:"schema_version"`
	WorkflowID    string      `json:"workflow_id"`
	Version       int         `json:"version"`
	Name          string      `json:"name"`
	Description   string      `json:"description,omitempty"`
	Owner         RoleBinding `json:"owner"`
	Trigger       TriggerSpec `json:"trigger"`
	State         StateSchema `json:"state"`
	Root          Node        `json:"root"`
}
