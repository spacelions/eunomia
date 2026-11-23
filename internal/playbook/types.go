package playbook

// DefaultMaxTurns and DefaultMaxDuration bound agent loops when omitted from frontmatter.
const (
	DefaultMaxTurns    = 30
	DefaultMaxDuration = "168h"
)

// Meta holds the lightweight deterministic guardrails declared in YAML frontmatter.
// The workflow logic itself lives in plain human-readable Markdown (Playbook.Body).
type Meta struct {
	PlaybookID        string   `yaml:"playbook_id" json:"playbook_id"`
	Version           int      `yaml:"version" json:"version"`
	Name              string   `yaml:"name" json:"name"`
	Description       string   `yaml:"description,omitempty" json:"description,omitempty"`
	AllowedConnectors []string `yaml:"allowed_connectors,omitempty" json:"allowed_connectors,omitempty"`
	RequiredTools     []string `yaml:"required_tools,omitempty" json:"required_tools,omitempty"`
	RequireApproval   bool     `yaml:"require_approval,omitempty" json:"require_approval,omitempty"`
	MaxTurns          int      `yaml:"max_turns,omitempty" json:"max_turns,omitempty"`
	MaxDuration       string   `yaml:"max_duration,omitempty" json:"max_duration,omitempty"`
}

// Playbook is an immutable versioned Markdown playbook: thin Go-enforced frontmatter
// guardrails paired with a plain-English Markdown SOP executed by the Temporal agent.
type Playbook struct {
	Meta `yaml:",inline" json:"meta"`
	Body string `yaml:"-" json:"body"`
}
