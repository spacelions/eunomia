package workflow_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/spacelions/eunomia/internal/workflow"
)

func TestGoldenFixture_EmployeeOnboarding_RoundTripAndHash(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "test", "fixtures", "employee_onboarding_v1.json")
	raw, err := os.ReadFile(fixturePath)
	require.NoError(t, err)

	def, err := workflow.ParseJSON(raw)
	require.NoError(t, err)

	assert.Equal(t, "internal-employee-onboarding", def.WorkflowID)
	assert.Equal(t, 1, def.Version)
	assert.True(t, def.State["temp_password"].Sensitive)

	// Canonical JSON round-trip must be completely lossless.
	canonical, err := def.CanonicalJSON()
	require.NoError(t, err)

	reparsed, err := workflow.ParseJSON(canonical)
	require.NoError(t, err)
	assert.Empty(t, cmp.Diff(def, reparsed, cmp.AllowUnexported(workflow.ValueExpr{})))

	// Deterministic ContentHash across reparsed copies.
	h1, err := def.ContentHash()
	require.NoError(t, err)
	h2, err := reparsed.ContentHash()
	require.NoError(t, err)
	assert.Len(t, h1, 64)
	assert.Equal(t, h1, h2)

	// Walk visits all 9 nodes in the golden fixture.
	var visited []string
	require.NoError(t, def.Walk(func(n *workflow.Node) error {
		visited = append(visited, n.ID)
		return nil
	}))
	assert.Empty(t, cmp.Diff([]string{
		"onboarding_root",
		"pick_email",
		"provision_google_user",
		"send_temp_credentials",
		"ai_tools_survey",
		"provision_ai_tools_branch",
		"manual_ai_provisioning",
		"closeout_notifications",
		"sync_notion_tracker",
		"notify_initiator",
	}, visited))
}

func TestArchetypeAndFieldTypeValid(t *testing.T) {
	for _, a := range []workflow.Archetype{
		workflow.ArchetypeRequestor,
		workflow.ArchetypeGroundTruthProvider,
		workflow.ArchetypeApprover,
		workflow.ArchetypeFulfiller,
		workflow.ArchetypeEscalationOwner,
		workflow.ArchetypeObserver,
	} {
		assert.True(t, a.Valid(), "archetype %s should be valid", a)
	}
	assert.False(t, workflow.Archetype("admin").Valid())

	for _, ft := range []workflow.FieldType{
		workflow.FieldTypeString,
		workflow.FieldTypeEmail,
		workflow.FieldTypeInt,
		workflow.FieldTypeFloat,
		workflow.FieldTypeBool,
		workflow.FieldTypeDate,
		workflow.FieldTypeStringList,
		workflow.FieldTypeEnum,
		workflow.FieldTypeFile,
		workflow.FieldTypeObject,
	} {
		assert.True(t, ft.Valid(), "field type %s should be valid", ft)
	}
	assert.False(t, workflow.FieldType("xml").Valid())
}

func TestRoleBinding_ParseAndRejectRawEmails(t *testing.T) {
	valid := []struct {
		raw    workflow.RoleBinding
		kind   workflow.RoleBindingKind
		target string
	}{
		{"run.initiator", workflow.RoleBindingInitiator, ""},
		{"workflow.owner", workflow.RoleBindingOwner, ""},
		{"role:it_ops", workflow.RoleBindingRole, "it_ops"},
		{"state.personal_email", workflow.RoleBindingState, "personal_email"},
	}
	for _, tc := range valid {
		kind, target, err := tc.raw.Parse()
		require.NoError(t, err, "binding %q", tc.raw)
		assert.Equal(t, tc.kind, kind)
		assert.Equal(t, tc.target, target)
	}

	invalid := []workflow.RoleBinding{
		"",
		"ops@spacelions.com",
		"role:",
		"role:IT-Ops",
		"state.",
		"state.BadField",
		"channel:general",
	}
	for _, raw := range invalid {
		assert.Error(t, raw.Validate(), "expected error for %q", raw)
	}
}

func TestValueExpr_JSONRoundTripAndValidation(t *testing.T) {
	// False, zero, and empty string literals must round-trip cleanly.
	for _, lit := range []any{false, "", json.Number("0"), "hello"} {
		ve := workflow.LiteralExpr(lit)
		assert.True(t, ve.IsLiteral())
		assert.False(t, ve.IsExpr())
		assert.Equal(t, lit, ve.Literal())

		data, err := json.Marshal(ve)
		require.NoError(t, err)
		var decoded workflow.ValueExpr
		require.NoError(t, json.Unmarshal(data, &decoded))
		assert.True(t, decoded.IsLiteral())
		assert.Equal(t, lit, decoded.Literal())
	}

	exprVal := workflow.Expr("state.employee_name")
	assert.True(t, exprVal.IsExpr())
	assert.Equal(t, "state.employee_name", exprVal.Expr())
	data, err := json.Marshal(exprVal)
	require.NoError(t, err)
	var decodedExpr workflow.ValueExpr
	require.NoError(t, json.Unmarshal(data, &decodedExpr))
	assert.Equal(t, "state.employee_name", decodedExpr.Expr())

	// Zero-value ValueExpr fails Validate and MarshalJSON.
	var empty workflow.ValueExpr
	require.Error(t, empty.Validate())
	_, err = json.Marshal(empty)
	require.Error(t, err)

	// Invalid JSON payloads.
	for _, bad := range []string{
		`{}`,
		`{"literal":"a","expr":"state.x"}`,
		`{"unknown":"a"}`,
		`{"expr":123}`,
		`{"literal":}`,
		`not-json`,
	} {
		var dst workflow.ValueExpr
		assert.Error(t, json.Unmarshal([]byte(bad), &dst), "expected error for %s", bad)
	}
}

func TestCompiledHTTPAction_Approver_Agent_ChildWorkflow_AndSwitchDefault(t *testing.T) {
	p := workflow.Expr("'Approve welcome draft for ' + state.customer")
	def := &workflow.WorkflowDef{
		SchemaVersion: workflow.SchemaVersionV1,
		WorkflowID:    "b2b-customer-onboarding",
		Version:       2,
		Name:          "B2B Customer Onboarding",
		Owner:         "role:csm",
		Trigger: workflow.TriggerSpec{
			Archetype: workflow.ArchetypeRequestor,
			InputKeys: []string{"customer", "tier"},
		},
		State: workflow.StateSchema{
			"customer":   {Type: workflow.FieldTypeString},
			"tier":       {Type: workflow.FieldTypeEnum, Options: []string{"pro", "enterprise"}},
			"ticket_url": {Type: workflow.FieldTypeString},
			"draft_body": {Type: workflow.FieldTypeString},
			"sso_ready":  {Type: workflow.FieldTypeBool},
		},
		Root: workflow.Node{
			ID:   "b2b_root",
			Name: "B2B Root",
			Type: workflow.NodeSequence,
			Sequence: &workflow.SequenceSpec{
				Steps: []workflow.Node{
					{
						ID:   "create_linear_ticket",
						Name: "Create Linear Ticket via Compiled HTTP",
						Type: workflow.NodeAction,
						Action: &workflow.ActionSpec{
							Connector: "linear",
							Operation: "http",
							HTTP: &workflow.HTTPActionSpec{
								Method: "POST",
								Path:   "/graphql",
								Headers: map[string]workflow.ValueExpr{
									"Content-Type": workflow.LiteralExpr("application/json"),
								},
								Query: map[string]workflow.ValueExpr{
									"version": workflow.LiteralExpr("1"),
								},
								Body: map[string]workflow.ValueExpr{
									"title": workflow.Expr("'Onboard ' + state.customer"),
								},
								IdempotencyHeader:  "Idempotency-Key",
								SuccessStatuses:    []int{200, 201},
								IdempotentStatuses: []int{409},
								RetryableStatuses:  []int{429, 500, 503},
								Extract: map[string]string{
									"ticket_url": "body.data.issueCreate.issue.url",
								},
							},
						},
					},
					{
						ID:   "draft_email_agent",
						Name: "Draft Welcome Email",
						Type: workflow.NodeAgent,
						Agent: &workflow.AgentSpec{
							Goal:     workflow.Expr("'Draft a welcome email for ' + state.customer"),
							MCPTools: []string{"notion.search"},
							MaxTurns: 4,
							OutputSchema: map[string]any{
								"type": "object",
							},
							Outputs: map[string]string{
								"draft_body": "email_body",
							},
						},
					},
					{
						ID:   "approve_draft",
						Name: "CSM Draft Gate",
						Type: workflow.NodeInteraction,
						Interaction: &workflow.InteractionSpec{
							Archetype: workflow.ArchetypeApprover,
							Channel:   "slack_card",
							Assignee:  "role:csm",
							DraftGate: true,
							Prompt:    &p,
							OnReject: &workflow.Node{
								ID:   "notify_rejection",
								Name: "Notify Rejection",
								Type: workflow.NodeAction,
								Action: &workflow.ActionSpec{
									Archetype: workflow.ArchetypeObserver,
									Connector: "slack",
									Operation: "post_message",
									Recipient: "run.initiator",
								},
							},
						},
					},
					{
						ID:   "tier_router",
						Name: "Route by Tier",
						Type: workflow.NodeSwitch,
						Switch: &workflow.SwitchSpec{
							Cases: []workflow.SwitchCase{
								{
									When: "state.tier == 'enterprise'",
									Node: workflow.Node{
										ID:   "run_sso_track",
										Name: "Run SSO Child Workflow",
										Type: workflow.NodeChildWorkflow,
										ChildWorkflow: &workflow.ChildWorkflowSpec{
											WorkflowID: "enterprise-sso-setup",
											Inputs: map[string]workflow.ValueExpr{
												"customer": workflow.Expr("state.customer"),
											},
											Outputs: map[string]string{
												"sso_ready": "completed",
											},
										},
									},
								},
							},
							Default: &workflow.Node{
								ID:   "pro_welcome",
								Name: "Standard Pro Welcome",
								Type: workflow.NodeAction,
								Action: &workflow.ActionSpec{
									Connector: "google_workspace",
									Operation: "send_email",
									Recipient: "run.initiator",
								},
							},
						},
					},
				},
			},
		},
	}

	raw, err := def.CanonicalJSON()
	require.NoError(t, err)

	parsed, err := workflow.ParseJSON(raw)
	require.NoError(t, err)
	assert.Empty(t, cmp.Diff(def, parsed, cmp.AllowUnexported(workflow.ValueExpr{})))
}

func TestParseJSON_RejectsUnknownFieldsAndTrailingData(t *testing.T) {
	_, err := workflow.ParseJSON([]byte(`{"schema_version":"v1","unknown_field":true}`))
	require.Error(t, err)

	fixturePath := filepath.Join("..", "..", "test", "fixtures", "employee_onboarding_v1.json")
	raw, err := os.ReadFile(fixturePath)
	require.NoError(t, err)

	_, err = workflow.ParseJSON(append(raw, []byte(`{"extra":1}`)...))
	require.ErrorContains(t, err, "trailing data")
}

func TestValidateSyntax_Errors(t *testing.T) {
	// Top-level & state & trigger validation errors.
	badTop := &workflow.WorkflowDef{
		SchemaVersion: "v99",
		WorkflowID:    "Bad_ID",
		Version:       0,
		Name:          "",
		Owner:         "raw@email.com",
		Trigger: workflow.TriggerSpec{
			Archetype: workflow.ArchetypeObserver,
			InputKeys: []string{"missing_key", "missing_key"},
		},
		State: workflow.StateSchema{
			"BadKey":  {Type: workflow.FieldType("bogus")},
			"bad_enm": {Type: workflow.FieldTypeEnum},
		},
		Root: workflow.Node{
			ID:   "BadNodeID",
			Name: "",
			Type: workflow.NodeType("unknown_type"),
		},
	}
	err := badTop.ValidateSyntax()
	require.Error(t, err)
	_, canonErr := badTop.CanonicalJSON()
	require.Error(t, canonErr)
	_, hashErr := badTop.ContentHash()
	require.Error(t, hashErr)

	// Helper to build a valid minimal WorkflowDef wrapping any root node.
	wrapRoot := func(root workflow.Node) *workflow.WorkflowDef {
		return &workflow.WorkflowDef{
			SchemaVersion: workflow.SchemaVersionV1,
			WorkflowID:    "test-wf",
			Version:       1,
			Name:          "Test",
			Owner:         "workflow.owner",
			Trigger: workflow.TriggerSpec{
				Archetype: workflow.ArchetypeRequestor,
				InputKeys: []string{"x"},
			},
			State: workflow.StateSchema{
				"x": {Type: workflow.FieldTypeString},
			},
			Root: root,
		}
	}

	var emptyExpr workflow.ValueExpr

	cases := []struct {
		name string
		root workflow.Node
	}{
		{
			name: "zero specs populated",
			root: workflow.Node{ID: "n", Name: "N", Type: workflow.NodeSequence},
		},
		{
			name: "two specs populated",
			root: workflow.Node{
				ID:       "n",
				Name:     "N",
				Type:     workflow.NodeSequence,
				Sequence: &workflow.SequenceSpec{},
				Parallel: &workflow.ParallelSpec{},
			},
		},
		{
			name: "mismatched spec for sequence",
			root: workflow.Node{ID: "n", Name: "N", Type: workflow.NodeSequence, Parallel: &workflow.ParallelSpec{}},
		},
		{
			name: "empty sequence",
			root: workflow.Node{ID: "n", Name: "N", Type: workflow.NodeSequence, Sequence: &workflow.SequenceSpec{}},
		},
		{
			name: "mismatched spec for parallel",
			root: workflow.Node{ID: "n", Name: "N", Type: workflow.NodeParallel, Sequence: &workflow.SequenceSpec{}},
		},
		{
			name: "parallel with < 2 branches",
			root: workflow.Node{
				ID:       "n",
				Name:     "N",
				Type:     workflow.NodeParallel,
				Parallel: &workflow.ParallelSpec{Branches: []workflow.Node{{ID: "a", Name: "A", Type: workflow.NodeAction, Action: &workflow.ActionSpec{Connector: "c", Operation: "op"}}}},
			},
		},
		{
			name: "mismatched spec for switch",
			root: workflow.Node{ID: "n", Name: "N", Type: workflow.NodeSwitch, Sequence: &workflow.SequenceSpec{}},
		},
		{
			name: "empty switch cases",
			root: workflow.Node{ID: "n", Name: "N", Type: workflow.NodeSwitch, Switch: &workflow.SwitchSpec{}},
		},
		{
			name: "switch case missing when",
			root: workflow.Node{
				ID:   "n",
				Name: "N",
				Type: workflow.NodeSwitch,
				Switch: &workflow.SwitchSpec{
					Cases: []workflow.SwitchCase{{When: "", Node: workflow.Node{ID: "a", Name: "A", Type: workflow.NodeAction, Action: &workflow.ActionSpec{Connector: "c", Operation: "op"}}}},
				},
			},
		},
		{
			name: "mismatched spec for action",
			root: workflow.Node{ID: "n", Name: "N", Type: workflow.NodeAction, Sequence: &workflow.SequenceSpec{}},
		},
		{
			name: "invalid action fields & http mismatch",
			root: workflow.Node{
				ID:   "n",
				Name: "N",
				Type: workflow.NodeAction,
				Action: &workflow.ActionSpec{
					Archetype: workflow.ArchetypeApprover,
					Connector: "",
					Operation: "create_user",
					Recipient: "bad@email.com",
					Inputs:    map[string]workflow.ValueExpr{"a": emptyExpr},
					Outputs:   map[string]string{"undeclared": "res"},
					HTTP:      &workflow.HTTPActionSpec{Method: "GET", Path: "/x"},
				},
			},
		},
		{
			name: "http operation missing http block",
			root: workflow.Node{
				ID:     "n",
				Name:   "N",
				Type:   workflow.NodeAction,
				Action: &workflow.ActionSpec{Connector: "linear", Operation: "http"},
			},
		},
		{
			name: "http action SSRF absolute URL, bad method, bad exprs & extract",
			root: workflow.Node{
				ID:   "n",
				Name: "N",
				Type: workflow.NodeAction,
				Action: &workflow.ActionSpec{
					Connector: "linear",
					Operation: "http",
					HTTP: &workflow.HTTPActionSpec{
						Method:  "TRACE",
						Path:    "https://evil.com/steal",
						Headers: map[string]workflow.ValueExpr{"h": emptyExpr},
						Query:   map[string]workflow.ValueExpr{"q": emptyExpr},
						Body:    map[string]workflow.ValueExpr{"b": emptyExpr},
						Extract: map[string]string{"undeclared": ""},
					},
				},
			},
		},
		{
			name: "mismatched spec for interaction",
			root: workflow.Node{ID: "n", Name: "N", Type: workflow.NodeInteraction, Sequence: &workflow.SequenceSpec{}},
		},
		{
			name: "invalid interaction fields, validators, SLA",
			root: workflow.Node{
				ID:   "n",
				Name: "N",
				Type: workflow.NodeInteraction,
				Interaction: &workflow.InteractionSpec{
					Archetype:  workflow.ArchetypeObserver,
					Channel:    "",
					Assignee:   "raw@email.com",
					EscalateTo: "raw2@email.com",
					Prompt:     &emptyExpr,
					Fields: []workflow.FormField{
						{
							StateKey: "undeclared",
							Label:    "",
							Validators: []workflow.ValidatorRef{
								{Connector: "", Validator: "", Params: map[string]workflow.ValueExpr{"p": emptyExpr}},
							},
						},
					},
					SLA: &workflow.SLAConfig{ReminderAfter: "notaduration", EscalateAfter: "-5m"},
				},
			},
		},
		{
			name: "mismatched spec for agent",
			root: workflow.Node{ID: "n", Name: "N", Type: workflow.NodeAgent, Sequence: &workflow.SequenceSpec{}},
		},
		{
			name: "invalid agent spec",
			root: workflow.Node{
				ID:   "n",
				Name: "N",
				Type: workflow.NodeAgent,
				Agent: &workflow.AgentSpec{
					Goal:     emptyExpr,
					MaxTurns: 0,
					Outputs:  map[string]string{"undeclared": "out"},
				},
			},
		},
		{
			name: "mismatched spec for child_workflow",
			root: workflow.Node{ID: "n", Name: "N", Type: workflow.NodeChildWorkflow, Sequence: &workflow.SequenceSpec{}},
		},
		{
			name: "invalid child_workflow spec",
			root: workflow.Node{
				ID:   "n",
				Name: "N",
				Type: workflow.NodeChildWorkflow,
				ChildWorkflow: &workflow.ChildWorkflowSpec{
					WorkflowID: "Bad_ID",
					Inputs:     map[string]workflow.ValueExpr{"a": emptyExpr},
					Outputs:    map[string]string{"undeclared": "out"},
				},
			},
		},
		{
			name: "unsupported node type with one spec",
			root: workflow.Node{ID: "n", Name: "N", Type: workflow.NodeType("loop"), Sequence: &workflow.SequenceSpec{}},
		},
		{
			name: "duplicate step IDs across sequence",
			root: workflow.Node{
				ID:   "root_seq",
				Name: "Root",
				Type: workflow.NodeSequence,
				Sequence: &workflow.SequenceSpec{
					Steps: []workflow.Node{
						{ID: "dup_step", Name: "A", Type: workflow.NodeAction, Action: &workflow.ActionSpec{Connector: "c", Operation: "op"}},
						{ID: "dup_step", Name: "B", Type: workflow.NodeAction, Action: &workflow.ActionSpec{Connector: "c", Operation: "op"}},
					},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := wrapRoot(tc.root).ValidateSyntax()
			require.Error(t, err, "expected syntax validation error for case %q", tc.name)
		})
	}
}
