package playbook

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

var (
	playbookIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	identRe      = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// ParseMarkdown parses a Markdown Playbook containing YAML frontmatter delimited by "---"
// followed by the Markdown SOP instructions body, and validates all frontmatter rails.
func ParseMarkdown(data []byte) (*Playbook, error) {
	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return nil, errors.New("playbook must start with YAML frontmatter delimited by '---'")
	}

	rest := normalized[len("---\n"):]
	idx := strings.Index(rest, "\n---\n")
	var fmRaw, body string
	if idx >= 0 {
		fmRaw = rest[:idx]
		body = strings.TrimSpace(rest[idx+len("\n---\n"):])
	} else if strings.HasSuffix(rest, "\n---") {
		fmRaw = strings.TrimSuffix(rest, "\n---")
		body = ""
	} else {
		return nil, errors.New("playbook frontmatter missing closing '---' delimiter")
	}

	dec := yaml.NewDecoder(strings.NewReader(fmRaw))
	dec.KnownFields(true)

	var fm Frontmatter
	if err := dec.Decode(&fm); err != nil {
		return nil, fmt.Errorf("decoding playbook frontmatter: %w", err)
	}

	pb := &Playbook{
		Frontmatter: fm,
		Body:        body,
	}
	if err := pb.ValidateSyntax(); err != nil {
		return nil, err
	}
	return pb, nil
}

// RenderMarkdown validates and renders the Playbook back to canonical YAML frontmatter + Markdown.
func (p *Playbook) RenderMarkdown() ([]byte, error) {
	if err := p.ValidateSyntax(); err != nil {
		return nil, err
	}
	var fmBuf bytes.Buffer
	enc := yaml.NewEncoder(&fmBuf)
	enc.SetIndent(2)
	if err := enc.Encode(&p.Frontmatter); err != nil {
		return nil, fmt.Errorf("encoding playbook frontmatter: %w", err)
	}
	_ = enc.Close()

	var out bytes.Buffer
	out.WriteString("---\n")
	out.Write(fmBuf.Bytes())
	out.WriteString("---\n\n")
	out.WriteString(strings.TrimSpace(p.Body))
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// ContentHash returns a deterministic hex-encoded SHA-256 digest of the canonicalized
// Frontmatter and normalized Markdown Body for immutable playbook versioning.
func (p *Playbook) ContentHash() (string, error) {
	if err := p.ValidateSyntax(); err != nil {
		return "", err
	}
	canonical := struct {
		Frontmatter Frontmatter `json:"frontmatter"`
		Body        string      `json:"body"`
	}{
		Frontmatter: p.Frontmatter,
		Body:        strings.TrimSpace(strings.ReplaceAll(p.Body, "\r\n", "\n")),
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// AllStateFields returns the merged map of Trigger.Inputs and State fields.
func (p *Playbook) AllStateFields() StateSchema {
	merged := make(StateSchema, len(p.Trigger.Inputs)+len(p.State))
	for k, v := range p.Trigger.Inputs {
		merged[k] = v
	}
	for k, v := range p.State {
		merged[k] = v
	}
	return merged
}

// AllowedOperations returns the set of whitelisted "<connector>.<operation>" identifiers
// across both native Go operations and build-time compiled HTTP operations.
func (p *Playbook) AllowedOperations() map[string]struct{} {
	allowed := make(map[string]struct{})
	for _, c := range p.Connectors {
		for _, op := range c.Operations {
			allowed[c.Name+"."+op] = struct{}{}
		}
		for op := range c.HTTPOperations {
			allowed[c.Name+"."+op] = struct{}{}
		}
	}
	return allowed
}

// CheckOutcomes evaluates the deterministic RequiredOutcomes completion gate before complete_run.
// Returns a slice of human/agent-readable missing requirements (empty slice means ready to complete).
func (p *Playbook) CheckOutcomes(populatedState map[string]bool, executedOps map[string]bool) []string {
	if p.RequiredOutcomes == nil {
		return nil
	}
	var missing []string
	for _, key := range p.RequiredOutcomes.StateKeys {
		if !populatedState[key] {
			missing = append(missing, fmt.Sprintf("missing required state key %q", key))
		}
	}
	for _, op := range p.RequiredOutcomes.Operations {
		if !executedOps[op] {
			missing = append(missing, fmt.Sprintf("required operation %q has not been executed", op))
		}
	}
	return missing
}

// ValidateSyntax checks structural and syntactic invariants of the Playbook frontmatter and body.
func (p *Playbook) ValidateSyntax() error {
	var errs []error

	if p.SchemaVersion != SchemaVersionV1 {
		errs = append(errs, fmt.Errorf("unsupported schema_version %q (expected %q)", p.SchemaVersion, SchemaVersionV1))
	}
	if !playbookIDRe.MatchString(p.PlaybookID) {
		errs = append(errs, fmt.Errorf("invalid playbook_id %q (must match ^[a-z][a-z0-9-]*$)", p.PlaybookID))
	}
	if p.Version < 1 {
		errs = append(errs, fmt.Errorf("version must be >= 1, got %d", p.Version))
	}
	if strings.TrimSpace(p.Name) == "" {
		errs = append(errs, errors.New("name is required"))
	}
	if err := p.Owner.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("owner: %w", err))
	}

	// Trigger & State fields.
	if p.Trigger.Archetype != ArchetypeRequestor {
		errs = append(errs, fmt.Errorf("trigger.archetype must be %q, got %q", ArchetypeRequestor, p.Trigger.Archetype))
	}
	validateFieldMap := func(prefix string, fields StateSchema) {
		for key, spec := range fields {
			if !identRe.MatchString(key) {
				errs = append(errs, fmt.Errorf("%s.%s: key must match ^[a-z][a-z0-9_]*$", prefix, key))
			}
			if !spec.Type.Valid() {
				errs = append(errs, fmt.Errorf("%s.%s: unsupported field type %q", prefix, key, spec.Type))
			}
			if spec.Type == FieldTypeEnum && len(spec.Options) == 0 {
				errs = append(errs, fmt.Errorf("%s.%s: enum field must define non-empty options", prefix, key))
			}
		}
	}
	validateFieldMap("trigger.inputs", p.Trigger.Inputs)
	validateFieldMap("state", p.State)

	for key := range p.State {
		if _, dup := p.Trigger.Inputs[key]; dup {
			errs = append(errs, fmt.Errorf("state.%s duplicates trigger.inputs.%s", key, key))
		}
	}
	allFields := p.AllStateFields()

	// Named role bindings.
	for name, role := range p.Roles {
		if !identRe.MatchString(name) {
			errs = append(errs, fmt.Errorf("roles.%s: name must match ^[a-z][a-z0-9_]*$", name))
		}
		if !role.Archetype.Valid() {
			errs = append(errs, fmt.Errorf("roles.%s: invalid archetype %q", name, role.Archetype))
		}
		if err := role.Binding.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("roles.%s.binding: %w", name, err))
		}
	}

	// Connector whitelist & optional compiled HTTP operations.
	seenConnectors := make(map[string]struct{}, len(p.Connectors))
	for i, c := range p.Connectors {
		if !identRe.MatchString(c.Name) {
			errs = append(errs, fmt.Errorf("connectors[%d].name %q must match ^[a-z][a-z0-9_]*$", i, c.Name))
			continue
		}
		if _, dup := seenConnectors[c.Name]; dup {
			errs = append(errs, fmt.Errorf("duplicate connector %q in connectors whitelist", c.Name))
		}
		seenConnectors[c.Name] = struct{}{}

		if len(c.Operations) == 0 && len(c.HTTPOperations) == 0 {
			errs = append(errs, fmt.Errorf("connector %q must whitelist at least one operation or http_operation", c.Name))
		}
		nativeOps := make(map[string]struct{}, len(c.Operations))
		for _, op := range c.Operations {
			if !identRe.MatchString(op) {
				errs = append(errs, fmt.Errorf("connector %q operation %q must match ^[a-z][a-z0-9_]*$", c.Name, op))
			}
			nativeOps[op] = struct{}{}
		}
		for _, res := range c.Resources {
			if !identRe.MatchString(res) {
				errs = append(errs, fmt.Errorf("connector %q resource alias %q must match ^[a-z][a-z0-9_]*$", c.Name, res))
			}
		}
		for opName, httpOp := range c.HTTPOperations {
			if !identRe.MatchString(opName) {
				errs = append(errs, fmt.Errorf("connector %q http_operation %q must match ^[a-z][a-z0-9_]*$", c.Name, opName))
			}
			if _, dup := nativeOps[opName]; dup {
				errs = append(errs, fmt.Errorf("connector %q http_operation %q conflicts with native operation of the same name", c.Name, opName))
			}
			if err := validateHTTPOperation(c.Name, opName, httpOp, allFields); err != nil {
				errs = append(errs, err)
			}
		}
	}

	// Hard execution limits.
	if p.Limits.MaxTurns < 1 {
		errs = append(errs, fmt.Errorf("limits.max_turns must be >= 1, got %d", p.Limits.MaxTurns))
	}
	if d, err := time.ParseDuration(p.Limits.MaxDuration); err != nil || d <= 0 {
		errs = append(errs, fmt.Errorf("limits.max_duration %q must be a positive duration", p.Limits.MaxDuration))
	}

	// Deterministic required_outcomes gate.
	if p.RequiredOutcomes != nil {
		allowedOps := p.AllowedOperations()
		for _, key := range p.RequiredOutcomes.StateKeys {
			if _, ok := allFields[key]; !ok {
				errs = append(errs, fmt.Errorf("required_outcomes.state_keys references undeclared field %q", key))
			}
		}
		for _, op := range p.RequiredOutcomes.Operations {
			if _, ok := allowedOps[op]; !ok {
				errs = append(errs, fmt.Errorf("required_outcomes.operations references non-whitelisted operation %q", op))
			}
		}
	}

	if strings.TrimSpace(p.Body) == "" {
		errs = append(errs, errors.New("markdown playbook body must not be empty"))
	}

	return errors.Join(errs...)
}

func validateHTTPOperation(connector, opName string, h HTTPOperationSpec, allFields StateSchema) error {
	var errs []error
	switch h.Method {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
	default:
		errs = append(errs, fmt.Errorf("connector %q http_operation %q: method %q must be GET, POST, PUT, PATCH, or DELETE", connector, opName, h.Method))
	}
	if !strings.HasPrefix(h.Path, "/") || strings.Contains(h.Path, "://") {
		errs = append(errs, fmt.Errorf("connector %q http_operation %q: path %q must be a relative path starting with '/' (no scheme/host allowed)", connector, opName, h.Path))
	}
	for stateKey, path := range h.Extract {
		if _, ok := allFields[stateKey]; !ok {
			errs = append(errs, fmt.Errorf("connector %q http_operation %q: extract references undeclared state key %q", connector, opName, stateKey))
		}
		if strings.TrimSpace(path) == "" {
			errs = append(errs, fmt.Errorf("connector %q http_operation %q: extract.%s path must not be empty", connector, opName, stateKey))
		}
	}
	return errors.Join(errs...)
}
