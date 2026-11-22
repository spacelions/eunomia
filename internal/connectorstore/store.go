package connectorstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

// ErrConnectorNotFound is returned when a tenant connector config does not exist.
var ErrConnectorNotFound = errors.New("connector config not found")

// Record represents non-secret per-tenant connector settings and semantic resource aliases.
// Credentials are never stored here; they live encrypted in tenant_secrets.
type Record struct {
	bun.BaseModel `bun:"table:tenant_connectors,alias:tc"`

	TenantID      string            `bun:"tenant_id,pk"`
	ConnectorType string            `bun:"connector_type,pk"`
	Enabled       bool              `bun:"enabled,notnull"`
	Settings      map[string]string `bun:"settings,type:jsonb,notnull"`
	Resources     map[string]string `bun:"resources,type:jsonb,notnull"`
	CreatedAt     time.Time         `bun:"created_at,notnull"`
	UpdatedAt     time.Time         `bun:"updated_at,notnull"`
}

// Store persists per-tenant connector configurations in PostgreSQL.
type Store struct {
	db *bun.DB
}

func New(db *bun.DB) *Store {
	return &Store{db: db}
}

func (s *Store) Upsert(ctx context.Context, rec Record) error {
	if strings.TrimSpace(rec.TenantID) == "" {
		return errors.New("tenant_id is required")
	}
	if strings.TrimSpace(rec.ConnectorType) == "" {
		return errors.New("connector_type is required")
	}
	if rec.Settings == nil {
		rec.Settings = map[string]string{}
	}
	if rec.Resources == nil {
		rec.Resources = map[string]string{}
	}
	now := time.Now().UTC()
	rec.CreatedAt = now
	rec.UpdatedAt = now

	_, err := s.db.NewInsert().
		Model(&rec).
		On("CONFLICT (tenant_id, connector_type) DO UPDATE").
		Set("enabled = EXCLUDED.enabled").
		Set("settings = EXCLUDED.settings").
		Set("resources = EXCLUDED.resources").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("upserting connector %s/%s: %w", rec.TenantID, rec.ConnectorType, err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, tenantID, connectorType string) (*Record, error) {
	rec := new(Record)
	err := s.db.NewSelect().
		Model(rec).
		Where("tenant_id = ? AND connector_type = ?", tenantID, connectorType).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s/%s", ErrConnectorNotFound, tenantID, connectorType)
		}
		return nil, fmt.Errorf("getting connector %s/%s: %w", tenantID, connectorType, err)
	}
	return rec, nil
}

func (s *Store) List(ctx context.Context, tenantID string) ([]Record, error) {
	var recs []Record
	err := s.db.NewSelect().
		Model(&recs).
		Where("tenant_id = ?", tenantID).
		OrderExpr("connector_type ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing connectors for tenant %q: %w", tenantID, err)
	}
	return recs, nil
}

func (s *Store) ListEnabled(ctx context.Context, tenantID string) ([]Record, error) {
	var recs []Record
	err := s.db.NewSelect().
		Model(&recs).
		Where("tenant_id = ? AND enabled = TRUE", tenantID).
		OrderExpr("connector_type ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing enabled connectors for tenant %q: %w", tenantID, err)
	}
	return recs, nil
}

func (s *Store) SetEnabled(ctx context.Context, tenantID, connectorType string, enabled bool) error {
	res, err := s.db.NewUpdate().
		Model((*Record)(nil)).
		Set("enabled = ?", enabled).
		Set("updated_at = ?", time.Now().UTC()).
		Where("tenant_id = ? AND connector_type = ?", tenantID, connectorType).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("updating connector %s/%s enabled=%v: %w", tenantID, connectorType, enabled, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: %s/%s", ErrConnectorNotFound, tenantID, connectorType)
	}
	return nil
}
