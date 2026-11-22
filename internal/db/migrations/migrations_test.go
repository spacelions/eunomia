package migrations

import (
	"regexp"
	"testing"
)

var timestampPrefixRe = regexp.MustCompile(`^\d{14}$`)

func TestMigrationsUseTimestampPrefix(t *testing.T) {
	all := Migrations.Sorted()
	if len(all) == 0 {
		t.Fatal("expected at least one migration")
	}
	for _, m := range all {
		if !timestampPrefixRe.MatchString(m.Name) {
			t.Errorf("migration %q (%s) must use a 14-digit YYYYMMDDHHMMSS timestamp prefix", m.Name, m.Comment)
		}
	}
}
