package migrations

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var timestampPrefixRe = regexp.MustCompile(`^\d{14}$`)

func TestMigrationsUseTimestampPrefix(t *testing.T) {
	all := Migrations.Sorted()
	require.NotEmpty(t, all)
	for _, m := range all {
		assert.Regexp(t, timestampPrefixRe, m.Name, "migration %s must use a 14-digit YYYYMMDDHHMMSS timestamp prefix", m.Comment)
	}
}
