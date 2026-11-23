package playbook

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

var (
	playbookIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	identRe      = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// ParseMarkdown parses a Markdown Playbook with YAML frontmatter delimited by "---",
// applies safe defaults for MaxTurns/MaxDuration, and validates all guardrails.
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

	var meta Meta
	if err := dec.Decode(&meta); err != nil {
		return nil, fmt.Errorf("decoding playbook frontmatter: %w", err)
	}

	if meta.MaxTurns == 0 {
		meta.MaxTurns = DefaultMaxTurns
	}
	if strings.TrimSpace(meta.MaxDuration) == "" {
		meta.MaxDuration = DefaultMaxDuration
	}

	pb := &Playbook{
		Meta: meta,
		Body: body,
	}
	if err := pb.Validate(); err != nil {
		return nil, err
	}
	return pb, nil
}

// RenderMarkdown validates and renders the Playbook to canonical YAML frontmatter + Markdown.
func (p *Playbook) RenderMarkdown() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	var fmBuf bytes.Buffer
	enc := yaml.NewEncoder(&fmBuf)
	enc.SetIndent(2)
	if err := enc.Encode(&p.Meta); err != nil {
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
// Meta and normalized Markdown Body for immutable playbook versioning.
func (p *Playbook) ContentHash() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	canonical := struct {
		Meta Meta   `json:"meta"`
		Body string `json:"body"`
	}{
		Meta: p.Meta,
		Body: strings.TrimSpace(strings.ReplaceAll(p.Body, "\r\n", "\n")),
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// AllowsConnector reports whether the named connector is permitted for this playbook.
// When AllowedConnectors is empty, any tenant-enabled connector is permitted.
func (p *Playbook) AllowsConnector(name string) bool {
	if len(p.AllowedConnectors) == 0 {
		return true
	}
	return slices.Contains(p.AllowedConnectors, name)
}

// CheckCompletion verifies that every entry in RequiredTools was executed before complete_run.
// Returns missing tool names (empty slice means ready to complete).
func (p *Playbook) CheckCompletion(executedTools map[string]bool) []string {
	var missing []string
	for _, tool := range p.RequiredTools {
		if !executedTools[tool] {
			missing = append(missing, tool)
		}
	}
	return missing
}

// Validate checks playbook metadata, execution bounds, connector allowlist, and non-empty body.
func (p *Playbook) Validate() error {
	var errs []error

	if !playbookIDRe.MatchString(p.PlaybookID) {
		errs = append(errs, fmt.Errorf("invalid playbook_id %q (must match ^[a-z][a-z0-9-]*$)", p.PlaybookID))
	}
	if p.Version < 1 {
		errs = append(errs, fmt.Errorf("version must be >= 1, got %d", p.Version))
	}
	if strings.TrimSpace(p.Name) == "" {
		errs = append(errs, errors.New("name is required"))
	}

	seenConns := make(map[string]struct{}, len(p.AllowedConnectors))
	for _, c := range p.AllowedConnectors {
		if !identRe.MatchString(c) {
			errs = append(errs, fmt.Errorf("allowed_connectors entry %q must match ^[a-z][a-z0-9_]*$", c))
			continue
		}
		if _, dup := seenConns[c]; dup {
			errs = append(errs, fmt.Errorf("duplicate connector %q in allowed_connectors", c))
		}
		seenConns[c] = struct{}{}
	}

	for _, tool := range p.RequiredTools {
		conn, op, ok := strings.Cut(tool, ".")
		if !ok || !identRe.MatchString(conn) || !identRe.MatchString(op) {
			errs = append(errs, fmt.Errorf("required_tools entry %q must have format <connector>.<operation>", tool))
			continue
		}
		if len(p.AllowedConnectors) > 0 {
			if _, allowed := seenConns[conn]; !allowed {
				errs = append(errs, fmt.Errorf("required_tools %q references connector %q not in allowed_connectors", tool, conn))
			}
		}
	}

	if p.MaxTurns < 1 {
		errs = append(errs, fmt.Errorf("max_turns must be >= 1, got %d", p.MaxTurns))
	}
	if d, err := time.ParseDuration(p.MaxDuration); err != nil || d <= 0 {
		errs = append(errs, fmt.Errorf("max_duration %q must be a positive duration", p.MaxDuration))
	}

	if strings.TrimSpace(p.Body) == "" {
		errs = append(errs, errors.New("markdown playbook body must not be empty"))
	}

	return errors.Join(errs...)
}
