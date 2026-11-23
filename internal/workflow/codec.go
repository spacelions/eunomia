package workflow

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

var (
	workflowIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	stepIDRe     = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	pathParamRe  = regexp.MustCompile(`\{([a-z][a-z0-9_]*)\}`)
)

// ParseJSON strictly decodes a WorkflowDef JSON document (rejecting unknown fields and trailing data)
// and validates its structural syntax.
func ParseJSON(data []byte) (*WorkflowDef, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var def WorkflowDef
	if err := dec.Decode(&def); err != nil {
		return nil, fmt.Errorf("decoding workflow JSON: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("decoding workflow JSON: unexpected trailing data")
	}

	if err := def.ValidateSyntax(); err != nil {
		return nil, err
	}
	return &def, nil
}

// CanonicalJSON validates and encodes WorkflowDef into deterministic compact JSON.
// Go's encoding/json sorts map keys lexicographically, guaranteeing stable digests.
func (w *WorkflowDef) CanonicalJSON() ([]byte, error) {
	if err := w.ValidateSyntax(); err != nil {
		return nil, err
	}
	return json.Marshal(w)
}

// ContentHash returns the hex-encoded SHA-256 digest of CanonicalJSON for immutable versioning.
func (w *WorkflowDef) ContentHash() (string, error) {
	raw, err := w.CanonicalJSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// Walk traverses every Node in the AST depth-first (including switch branches and approver on_reject).
func (w *WorkflowDef) Walk(fn func(*Node) error) error {
	return walkNode(&w.Root, fn)
}

func walkNode(n *Node, fn func(*Node) error) error {
	if err := fn(n); err != nil {
		return err
	}
	switch n.Type {
	case NodeSequence:
		if n.Sequence != nil {
			for i := range n.Sequence.Steps {
				if err := walkNode(&n.Sequence.Steps[i], fn); err != nil {
					return err
				}
			}
		}
	case NodeParallel:
		if n.Parallel != nil {
			for i := range n.Parallel.Branches {
				if err := walkNode(&n.Parallel.Branches[i], fn); err != nil {
					return err
				}
			}
		}
	case NodeSwitch:
		if n.Switch != nil {
			for i := range n.Switch.Cases {
				if err := walkNode(&n.Switch.Cases[i].Node, fn); err != nil {
					return err
				}
			}
			if n.Switch.Default != nil {
				if err := walkNode(n.Switch.Default, fn); err != nil {
					return err
				}
			}
		}
	case NodeAction:
		if n.Action != nil && n.Action.OnError != nil {
			if err := walkNode(n.Action.OnError, fn); err != nil {
				return err
			}
		}
	case NodeInteraction:
		if n.Interaction != nil && n.Interaction.OnReject != nil {
			if err := walkNode(n.Interaction.OnReject, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidateSyntax checks structural and syntactic invariants of WorkflowDef.
// Semantic checks against live ConnectorRegistry capabilities and expr-lang type checking
// belong in the Milestone 3 2-Tier Static Linter.
func (w *WorkflowDef) ValidateSyntax() error {
	var errs []error

	if w.SchemaVersion != SchemaVersionV1 {
		errs = append(errs, fmt.Errorf("unsupported schema_version %q (expected %q)", w.SchemaVersion, SchemaVersionV1))
	}
	if !workflowIDRe.MatchString(w.WorkflowID) {
		errs = append(errs, fmt.Errorf("invalid workflow_id %q (must match ^[a-z][a-z0-9-]*$)", w.WorkflowID))
	}
	if w.Version < 1 {
		errs = append(errs, fmt.Errorf("version must be >= 1, got %d", w.Version))
	}
	if strings.TrimSpace(w.Name) == "" {
		errs = append(errs, errors.New("name is required"))
	}
	if err := w.Owner.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("owner: %w", err))
	}

	// Validate State schema.
	for key, spec := range w.State {
		if !stepIDRe.MatchString(key) {
			errs = append(errs, fmt.Errorf("state key %q must match ^[a-z][a-z0-9_]*$", key))
		}
		if !spec.Type.Valid() {
			errs = append(errs, fmt.Errorf("state.%s: unsupported field type %q", key, spec.Type))
		}
		if spec.Type == FieldTypeEnum && len(spec.Options) == 0 {
			errs = append(errs, fmt.Errorf("state.%s: enum field must define non-empty options", key))
		}
	}

	// Validate Trigger.
	if w.Trigger.Archetype != ArchetypeRequestor {
		errs = append(errs, fmt.Errorf("trigger.archetype must be %q, got %q", ArchetypeRequestor, w.Trigger.Archetype))
	}
	seenTriggerKeys := make(map[string]struct{}, len(w.Trigger.InputKeys))
	for _, k := range w.Trigger.InputKeys {
		if _, dup := seenTriggerKeys[k]; dup {
			errs = append(errs, fmt.Errorf("trigger.input_keys contains duplicate key %q", k))
		}
		seenTriggerKeys[k] = struct{}{}
		if _, ok := w.State[k]; !ok {
			errs = append(errs, fmt.Errorf("trigger.input_keys references undeclared state key %q", k))
		}
	}

	// Walk and validate every AST node.
	seenIDs := make(map[string]struct{})
	_ = w.Walk(func(n *Node) error {
		if !stepIDRe.MatchString(n.ID) {
			errs = append(errs, fmt.Errorf("node id %q must match ^[a-z][a-z0-9_]*$", n.ID))
		} else if _, dup := seenIDs[n.ID]; dup {
			errs = append(errs, fmt.Errorf("duplicate node id %q", n.ID))
		} else {
			seenIDs[n.ID] = struct{}{}
		}
		if strings.TrimSpace(n.Name) == "" {
			errs = append(errs, fmt.Errorf("node %q: name is required", n.ID))
		}
		if err := validateNodeSpec(n, w.State); err != nil {
			errs = append(errs, fmt.Errorf("node %q: %w", n.ID, err))
		}
		return nil
	})

	return errors.Join(errs...)
}

func countPopulatedSpecs(n *Node) int {
	count := 0
	if n.Sequence != nil {
		count++
	}
	if n.Parallel != nil {
		count++
	}
	if n.Switch != nil {
		count++
	}
	if n.Wait != nil {
		count++
	}
	if n.Action != nil {
		count++
	}
	if n.Interaction != nil {
		count++
	}
	if n.Agent != nil {
		count++
	}
	if n.ChildWorkflow != nil {
		count++
	}
	return count
}

func validateNodeSpec(n *Node, state StateSchema) error {
	if got := countPopulatedSpecs(n); got != 1 {
		return fmt.Errorf("must populate exactly one spec block matching type %q, found %d", n.Type, got)
	}

	switch n.Type {
	case NodeSequence:
		if n.Sequence == nil {
			return errors.New("type sequence requires sequence block")
		}
		if len(n.Sequence.Steps) == 0 {
			return errors.New("sequence.steps must not be empty")
		}
		return nil

	case NodeParallel:
		if n.Parallel == nil {
			return errors.New("type parallel requires parallel block")
		}
		if len(n.Parallel.Branches) < 2 {
			return errors.New("parallel.branches must contain at least 2 branches")
		}
		return nil

	case NodeSwitch:
		if n.Switch == nil {
			return errors.New("type switch requires switch block")
		}
		if len(n.Switch.Cases) == 0 {
			return errors.New("switch.cases must not be empty")
		}
		for i, c := range n.Switch.Cases {
			if strings.TrimSpace(c.When) == "" {
				return fmt.Errorf("switch.cases[%d].when must not be empty", i)
			}
		}
		return nil

	case NodeWait:
		if n.Wait == nil {
			return errors.New("type wait requires wait block")
		}
		return validateWaitSpec(n.Wait)

	case NodeAction:
		if n.Action == nil {
			return errors.New("type action requires action block")
		}
		return validateActionSpec(n.Action, state)

	case NodeInteraction:
		if n.Interaction == nil {
			return errors.New("type interaction requires interaction block")
		}
		return validateInteractionSpec(n.Interaction, state)

	case NodeAgent:
		if n.Agent == nil {
			return errors.New("type agent requires agent block")
		}
		return validateAgentSpec(n.Agent, state)

	case NodeChildWorkflow:
		if n.ChildWorkflow == nil {
			return errors.New("type child_workflow requires child_workflow block")
		}
		return validateChildWorkflowSpec(n.ChildWorkflow, state)

	default:
		return fmt.Errorf("unsupported node type %q", n.Type)
	}
}

func validateWaitSpec(ws *WaitSpec) error {
	hasDuration := strings.TrimSpace(ws.Duration) != ""
	hasUntil := ws.Until != nil
	if hasDuration == hasUntil {
		return errors.New("wait must set exactly one of duration or until")
	}
	if hasDuration {
		if d, err := time.ParseDuration(ws.Duration); err != nil || d <= 0 {
			return fmt.Errorf("wait.duration %q must be a positive duration", ws.Duration)
		}
		return nil
	}
	if err := ws.Until.Validate(); err != nil {
		return fmt.Errorf("wait.until: %w", err)
	}
	return nil
}

func validateActionSpec(a *ActionSpec, state StateSchema) error {
	var errs []error
	if a.Archetype != "" && a.Archetype != ArchetypeObserver {
		errs = append(errs, fmt.Errorf("action.archetype may only be empty or %q, got %q", ArchetypeObserver, a.Archetype))
	}
	if strings.TrimSpace(a.Connector) == "" {
		errs = append(errs, errors.New("action.connector is required"))
	}
	if strings.TrimSpace(a.Operation) == "" {
		errs = append(errs, errors.New("action.operation is required"))
	}
	if a.Recipient != "" {
		if err := a.Recipient.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("action.recipient: %w", err))
		}
	}
	for k, v := range a.Inputs {
		if err := v.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("action.inputs.%s: %w", k, err))
		}
	}
	for stateKey := range a.Outputs {
		if _, ok := state[stateKey]; !ok {
			errs = append(errs, fmt.Errorf("action.outputs references undeclared state key %q", stateKey))
		}
	}

	if a.Operation == "http" {
		if a.HTTP == nil {
			errs = append(errs, errors.New("action.http block is required when operation is \"http\""))
		} else if err := validateHTTPActionSpec(a.HTTP, state); err != nil {
			errs = append(errs, err)
		}
	} else if a.HTTP != nil {
		errs = append(errs, fmt.Errorf("action.http block is only allowed when operation is \"http\" (got %q)", a.Operation))
	}

	return errors.Join(errs...)
}

func validateHTTPActionSpec(h *HTTPActionSpec, state StateSchema) error {
	var errs []error

	switch h.Method {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
	default:
		errs = append(errs, fmt.Errorf("http.method %q must be GET, POST, PUT, PATCH, or DELETE", h.Method))
	}

	if !strings.HasPrefix(h.Path, "/") || strings.Contains(h.Path, "://") {
		errs = append(errs, fmt.Errorf("http.path %q must be a relative path starting with '/' (no scheme/host allowed)", h.Path))
	}

	// Ensure every {param} placeholder in Path has a matching PathParams entry and vice versa.
	placeholders := make(map[string]struct{})
	for _, m := range pathParamRe.FindAllStringSubmatch(h.Path, -1) {
		name := m[1]
		placeholders[name] = struct{}{}
		if _, ok := h.PathParams[name]; !ok {
			errs = append(errs, fmt.Errorf("http.path placeholder {%s} is missing from http.path_params", name))
		}
	}
	for k, v := range h.PathParams {
		if _, ok := placeholders[k]; !ok {
			errs = append(errs, fmt.Errorf("http.path_params.%s is not referenced in http.path %q", k, h.Path))
		}
		if err := v.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("http.path_params.%s: %w", k, err))
		}
	}

	for k, v := range h.Headers {
		if err := v.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("http.headers.%s: %w", k, err))
		}
	}
	for k, v := range h.Query {
		if err := v.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("http.query.%s: %w", k, err))
		}
	}
	if h.Body != nil {
		if err := h.Body.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("http.body: %w", err))
		}
	}
	for stateKey, expr := range h.Extract {
		if _, ok := state[stateKey]; !ok {
			errs = append(errs, fmt.Errorf("http.extract references undeclared state key %q", stateKey))
		}
		if strings.TrimSpace(expr) == "" {
			errs = append(errs, fmt.Errorf("http.extract.%s expression must not be empty", stateKey))
		}
	}

	return errors.Join(errs...)
}

func validateInteractionSpec(in *InteractionSpec, state StateSchema) error {
	var errs []error

	switch in.Archetype {
	case ArchetypeGroundTruthProvider, ArchetypeApprover, ArchetypeFulfiller:
	default:
		errs = append(errs, fmt.Errorf("interaction.archetype must be ground_truth_provider, approver, or fulfiller, got %q", in.Archetype))
	}
	if strings.TrimSpace(in.Channel) == "" {
		errs = append(errs, errors.New("interaction.channel is required"))
	}
	if err := in.Assignee.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("interaction.assignee: %w", err))
	}
	if in.EscalateTo != "" {
		if err := in.EscalateTo.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("interaction.escalate_to: %w", err))
		}
	}
	if in.Prompt != nil {
		if err := in.Prompt.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("interaction.prompt: %w", err))
		}
	}

	validateFields := func(prefix string, fields []FormField) {
		for i, f := range fields {
			if _, ok := state[f.StateKey]; !ok {
				errs = append(errs, fmt.Errorf("%s[%d].state_key references undeclared state key %q", prefix, i, f.StateKey))
			}
			if strings.TrimSpace(f.Label) == "" {
				errs = append(errs, fmt.Errorf("%s[%d].label is required", prefix, i))
			}
			for j, v := range f.Validators {
				if strings.TrimSpace(v.Connector) == "" || strings.TrimSpace(v.Validator) == "" {
					errs = append(errs, fmt.Errorf("%s[%d].validators[%d] requires connector and validator", prefix, i, j))
				}
				for pk, pv := range v.Params {
					if err := pv.Validate(); err != nil {
						errs = append(errs, fmt.Errorf("%s[%d].validators[%d].params.%s: %w", prefix, i, j, pk, err))
					}
				}
			}
		}
	}
	validateFields("interaction.fields", in.Fields)
	validateFields("interaction.receipt_fields", in.ReceiptFields)

	if in.SLA != nil {
		if in.SLA.ReminderAfter != "" {
			if d, err := time.ParseDuration(in.SLA.ReminderAfter); err != nil || d <= 0 {
				errs = append(errs, fmt.Errorf("interaction.sla.reminder_after %q must be a positive duration", in.SLA.ReminderAfter))
			}
		}
		if in.SLA.EscalateAfter != "" {
			if d, err := time.ParseDuration(in.SLA.EscalateAfter); err != nil || d <= 0 {
				errs = append(errs, fmt.Errorf("interaction.sla.escalate_after %q must be a positive duration", in.SLA.EscalateAfter))
			}
		}
	}

	return errors.Join(errs...)
}

func validateAgentSpec(ag *AgentSpec, state StateSchema) error {
	var errs []error
	if err := ag.Goal.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("agent.goal: %w", err))
	}
	if ag.MaxTurns < 1 {
		errs = append(errs, fmt.Errorf("agent.max_turns must be >= 1, got %d", ag.MaxTurns))
	}
	for stateKey := range ag.Outputs {
		if _, ok := state[stateKey]; !ok {
			errs = append(errs, fmt.Errorf("agent.outputs references undeclared state key %q", stateKey))
		}
	}
	return errors.Join(errs...)
}

func validateChildWorkflowSpec(cw *ChildWorkflowSpec, state StateSchema) error {
	var errs []error
	if !workflowIDRe.MatchString(cw.WorkflowID) {
		errs = append(errs, fmt.Errorf("child_workflow.workflow_id %q must match ^[a-z][a-z0-9-]*$", cw.WorkflowID))
	}
	for k, v := range cw.Inputs {
		if err := v.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("child_workflow.inputs.%s: %w", k, err))
		}
	}
	for stateKey := range cw.Outputs {
		if _, ok := state[stateKey]; !ok {
			errs = append(errs, fmt.Errorf("child_workflow.outputs references undeclared state key %q", stateKey))
		}
	}
	return errors.Join(errs...)
}
