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

func TestGoldenFixture_EmployeeOnboarding_RoundTripHashAndGates(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "test", "fixtures", "employee_onboarding_v1.md")
	raw, err := os.ReadFile(fixturePath)
	require.NoError(t, err)

	pb, err := playbook.ParseMarkdown(raw)
	require.NoError(t, err)

	assert.Equal(t, "internal-employee-onboarding", pb.PlaybookID)
	assert.Equal(t, 1, pb.Version)
	assert.Equal(t, 30, pb.MaxTurns)
	assert.Equal(t, "168h", pb.MaxDuration)
	assert.Contains(t, pb.Body, "# Internal Employee Onboarding")

	// Connector allowlist checks.
	assert.True(t, pb.AllowsConnector("google_workspace"))
	assert.True(t, pb.AllowsConnector("notion"))
	assert.False(t, pb.AllowsConnector("slack"))

	// Render -> re-parse round-trip is lossless.
	rendered, err := pb.RenderMarkdown()
	require.NoError(t, err)

	reparsed, err := playbook.ParseMarkdown(rendered)
	require.NoError(t, err)
	assert.Empty(t, cmp.Diff(pb, reparsed))

	// Deterministic ContentHash across reparsed copies.
	h1, err := pb.ContentHash()
	require.NoError(t, err)
	h2, err := reparsed.ContentHash()
	require.NoError(t, err)
	assert.Len(t, h1, 64)
	assert.Equal(t, h1, h2)

	// Completion gate blocks premature finish and passes when required_tools ran.
	missing := pb.CheckCompletion(map[string]bool{"google_workspace.create_user": true})
	assert.Equal(t, []string{"notion.upsert_db_row"}, missing)

	missing = pb.CheckCompletion(map[string]bool{
		"google_workspace.create_user": true,
		"notion.upsert_db_row":         true,
	})
	assert.Empty(t, missing)
}

func TestParseMarkdown_DefaultsAndOpenConnectors(t *testing.T) {
	md := `---
playbook_id: quick-checkin
version: 1
name: Quick Check-in
---

# Quick Check-in

Send a friendly check-in email to the initiator after 7 days.
`
	pb, err := playbook.ParseMarkdown([]byte(md))
	require.NoError(t, err)
	assert.Equal(t, playbook.DefaultMaxTurns, pb.MaxTurns)
	assert.Equal(t, playbook.DefaultMaxDuration, pb.MaxDuration)
	assert.True(t, pb.AllowsConnector("any_enabled_connector"))
	assert.Empty(t, pb.CheckCompletion(nil))
}

func TestParseMarkdown_DelimiterAndUnknownFieldErrors(t *testing.T) {
	cases := []struct {
		name string
		md   string
	}{
		{"missing opening delimiter", "# No frontmatter\n"},
		{"missing closing delimiter", "---\nplaybook_id: x\n# No close\n"},
		{"empty body with EOF delimiter", "---\nplaybook_id: x\nversion: 1\nname: X\n---"},
		{"unknown yaml key", "---\nplaybook_id: x\nversion: 1\nname: X\nunknown_key: true\n---\nBody\n"},
		{"invalid yaml syntax", "---\n: [bad\n---\nBody\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := playbook.ParseMarkdown([]byte(tc.md))
			require.Error(t, err)
		})
	}
}

func TestValidate_Errors(t *testing.T) {
	bad := &playbook.Playbook{
		Meta: playbook.Meta{
			PlaybookID:        "Bad_ID",
			Version:           0,
			Name:              "",
			AllowedConnectors: []string{"BadConn", "notion", "notion"},
			RequiredTools:     []string{"bad_format", "slack.post_message"},
			MaxTurns:          0,
			MaxDuration:       "-5m",
		},
		Body: "   ",
	}

	require.Error(t, bad.Validate())
	_, renderErr := bad.RenderMarkdown()
	require.Error(t, renderErr)
	_, hashErr := bad.ContentHash()
	require.Error(t, hashErr)
}
