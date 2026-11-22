package migrations

import (
	"embed"

	"github.com/uptrace/bun/migrate"
)

// Migrations discovers all embedded SQL migration files in this package.
var Migrations = migrate.NewMigrations()

//go:embed *.sql
var sqlMigrations embed.FS

func init() {
	if err := Migrations.Discover(sqlMigrations); err != nil {
		panic(err)
	}
}
