package playbook_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/spacelions/eunomia/internal/playbook"
)

func TestGoldenFixture_EmployeeOnboarding_RoundTripHashAndOutcomes(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "test", "fixtures", "employee_onboarding_v1.md")
	raw, err := os.ReadFile(fixturePath)
	require.NoError(t, err)

	pb, err := playbook.ParseMarkdown(raw)
	require.NoError(t, err)

	assert.Equal(t, "internal-employee-onboarding", pb.PlaybookID)
	assert.Equal(t, 1, pb.Version)
	assert.True(t, pb.State["temp_password"].Sensitive)
	assert.Contains(t, pb.Body, "# Internal Employee Onboarding")

	// Render -> re-parse must be completely lossless.
	rendered, err := pb.RenderMarkdown()
	require.NoError(t, err)

	reparsed, err := playbook.ParseMarkdown(rendered)
	require.NoError(t, err)
	assert.Empty(t, cmp.Diff(pb, reparsed))

	// ContentHash must be deterministic across reparsed copies.
	h1, err := pb.ContentHash()
	require.NoError(t, err)
	h2, err := reparsed.ContentHash()
	require.NoError(t, err)
	assert.Len(t, h1, 64)
	assert.Equal(t, h1, h2)

	// Deterministic completion gate blocks premature complete_run and passes when satisfied.
	missing := pb.CheckOutcomes(map[string]bool{}, map[string]bool{"google_workspace.create_user": true})
	assert.Len(t, missing, 2)
	assert.Contains(t, missing[0], "work_email")
	assert.Contains(t, missing[1], "notion.upsert_db_row")

	missing = pb.CheckOutcomes(
		map[string]bool{"work_email": true},
		map[string]bool{"google_workspace.create_user": true, "notion.upsert_db_row": true},
	)
	assert.Empty(t, missing)
}

func TestCompiledHTTPOperations_AndNilOutcomes(t *testing.T) {
	md := `---
schema_version: v1
playbook_id: b2b-ticket-sync
version: 1
name: B2B Ticket Sync
owner: role:csm
trigger:
  archetype: requestor
  inputs:
    customer: { type: string }
    tier: { type: enum, options: [pro, enterprise] }
state:
  ticket_url: { type: string }
connectors:
  - name: linear
    http_operations:
      create_issue:
        method: POST
        path: /v1/teams/{team_id}/issues
        description: Create a Linear issue via build-time compiled HTTP call
        idempotency_header: Idempotency-Key
        success_statuses: [200, 201]
        idempotent_statuses: [409]
        retryable_statuses: [429, 500, 503]
        extract:
          ticket_url: body.data.issueCreate.issue.url
limits:
  max_turns: 10
  max_duration: 24h
---

# B2B Ticket Sync

Create a Linear onboarding ticket for the customer using ` + "`linear.create_issue`" + `.
`
	pb, err := playbook.ParseMarkdown([]byte(md))
	require.NoError(t, err)

	ops := pb.AllowedOperations()
	_, ok := ops["linear.create_issue"]
	assert.True(t, ok)
	assert.Empty(t, pb.CheckOutcomes(nil, nil))
}

func TestArchetypeAndFieldTypeValid(t *testing.T) {
	for _, a := range []playbook.Archetype{
		playbook.ArchetypeRequestor,
		playbook.ArchetypeGroundTruthProvider,
		playbook.ArchetypeApprover,
		playbook.ArchetypeFulfiller,
		playbook.ArchetypeEscalationOwner,
		playbook.ArchetypeObserver,
	} {
		assert.True(t, a.Valid())
	}
	assert.False(t, playbook.Archetype("admin").Valid())

	for _, ft := range []playbook.FieldType{
		playbook.FieldTypeString,
		playbook.FieldTypeEmail,
		playbook.FieldTypeInt,
		playbook.FieldTypeFloat,
		playbook.FieldTypeBool,
		playbook.FieldTypeDate,
		playbook.FieldTypeStringList,
		playbook.FieldTypeEnum,
		playbook.FieldTypeFile,
		playbook.FieldTypeObject,
	} {
		assert.True(t, ft.Valid())
	}
	assert.False(t, playbook.FieldType("xml").Valid())
}

func TestRoleBinding_ParseAndRejectRawEmails(t *testing.T) {
	valid := []struct {
		raw    playbook.RoleBinding
		kind   playbook.RoleBindingKind
		target string
	}{
		{"run.initiator", playbook.RoleBindingInitiator, ""},
		{"workflow.owner", playbook.RoleBindingOwner, ""},
		{"role:it_ops", playbook.RoleBindingRole, "it_ops"},
		{"state.personal_email", playbook.RoleBindingState, "personal_email"},
	}
	for _, tc := range valid {
		kind, target, err := tc.raw.Parse()
		require.NoError(t, err)
		assert.Equal(t, tc.kind, kind)
		assert.Equal(t, tc.target, target)
	}

	for _, bad := range []playbook.RoleBinding{
		"",
		"ops@spacelions.com",
		"role:",
		"role:Bad-Name",
		"state.",
		"state.BadField",
		"channel:ops",
	} {
		assert.Error(t, bad.Validate(), "expected error for %q", bad)
	}
}

func TestParseMarkdown_DelimiterAndUnknownFieldErrors(t *testing.T) {
	cases := []struct {
		name string
		md   string
	}{
		{"missing opening delimiter", "# No frontmatter\n"},
		{"missing closing delimiter", "---\nschema_version: v1\n# No close\n"},
		{"empty body with EOF delimiter", "---\nschema_version: v1\nplaybook_id: x\nversion: 1\nname: X\nowner: workflow.owner\ntrigger:\n  archetype: requestor\nlimits:\n  max_turns: 5\n  max_duration: 1h\n---"},
		{"unknown yaml key", "---\nschema_version: v1\nunknown_key: true\n---\nBody\n"},
		{"invalid yaml syntax", "---\n: [bad\n---\nBody\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := playbook.ParseMarkdown([]byte(tc.md))
			require.Error(t, err)
		})
	}
}

func TestValidateSyntax_Errors(t *testing.T) {
	bad := &playbook.Playbook{
		Frontmatter: playbook.Frontmatter{
			SchemaVersion: "v99",
			PlaybookID:    "Bad_ID",
			Version:       0,
			Name:          "",
			Owner:         "raw@email.com",
			Trigger: playbook.TriggerSpec{
				Archetype: playbook.ArchetypeObserver,
				Inputs: playbook.StateSchema{
					"BadKey":  {Type: playbook.FieldType("bogus")},
					"bad_enm": {Type: playbook.FieldTypeEnum},
					"dup_key": {Type: playbook.FieldTypeString},
				},
			},
			State: playbook.StateSchema{
				"dup_key": {Type: playbook.FieldTypeString},
			},
			Roles: map[string]playbook.RoleSpec{
				"BadRole": {Archetype: playbook.Archetype("invalid"), Binding: "raw@email.com"},
			},
			Connectors: []playbook.ConnectorAllowSpec{
				{Name: "BadConn"},
				{Name: "empty_conn"},
				{
					Name:       "linear",
					Operations: []string{"BadOp", "dup_op"},
					Resources:  []string{"BadRes"},
					HTTPOperations: map[string]playbook.HTTPOperationSpec{
						"BadHTTP": {Method: "GET", Path: "/ok"},
						"dup_op": {
							Method:  "TRACE",
							Path:    "https://evil.com/steal",
							Extract: map[string]string{"undeclared": ""},
						},
					},
				},
				{Name: "linear", Operations: []string{"ok_op"}},
			},
			Limits: playbook.LimitsSpec{
				MaxTurns:    0,
				MaxDuration: "-5m",
			},
			RequiredOutcomes: &playbook.RequiredOutcomesSpec{
				StateKeys:  []string{"missing_state"},
				Operations: []string{"slack.post_message"},
			},
		},
		Body: "   ",
	}

	err := bad.ValidateSyntax()
	require.Error(t, err)
	_, renderErr := bad.RenderMarkdown()
	require.Error(t, renderErr)
	_, hashErr := bad.ContentHash()
	require.Error(t, hashErr)
}
