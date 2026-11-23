package playbook

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

var (
	// ErrPlaybookNotFound is returned when a published playbook version does not exist.
	ErrPlaybookNotFound = errors.New("playbook not found")

	// ErrDraftNotFound is returned when a working draft does not exist.
	ErrDraftNotFound = errors.New("playbook draft not found")
)

// Record is an immutable published playbook version persisted in PostgreSQL.
type Record struct {
	bun.BaseModel `bun:"table:playbooks,alias:p"`

	TenantID    string    `bun:"tenant_id,pk" json:"tenant_id"`
	PlaybookID  string    `bun:"playbook_id,pk" json:"playbook_id"`
	Version     int       `bun:"version,pk" json:"version"`
	Name        string    `bun:"name,notnull" json:"name"`
	Description string    `bun:"description,notnull" json:"description,omitempty"`
	Meta        Meta      `bun:"meta,type:jsonb,notnull" json:"meta"`
	RawMarkdown string    `bun:"raw_markdown,notnull" json:"raw_markdown"`
	ContentHash string    `bun:"content_hash,notnull" json:"content_hash"`
	CreatedAt   time.Time `bun:"created_at,notnull" json:"created_at"`
}

// Playbook parses the stored RawMarkdown back into a validated Playbook.
func (r *Record) Playbook() (*Playbook, error) {
	return ParseMarkdown([]byte(r.RawMarkdown))
}

// Draft is a mutable working copy of a playbook prior to publishing an immutable version.
type Draft struct {
	bun.BaseModel `bun:"table:playbook_drafts,alias:pd"`

	TenantID    string    `bun:"tenant_id,pk" json:"tenant_id"`
	PlaybookID  string    `bun:"playbook_id,pk" json:"playbook_id"`
	RawMarkdown string    `bun:"raw_markdown,notnull" json:"raw_markdown"`
	UpdatedAt   time.Time `bun:"updated_at,notnull" json:"updated_at"`
}

// Store manages mutable playbook drafts and append-only immutable published versions in PostgreSQL.
type Store struct {
	db *bun.DB
}

func NewStore(db *bun.DB) *Store {
	return &Store{db: db}
}

// SaveDraft upserts the mutable working draft for (tenantID, playbookID).
// Drafts do not need to pass full Playbook validation so users can save work-in-progress edits.
func (s *Store) SaveDraft(ctx context.Context, tenantID, playbookID, rawMarkdown string) (*Draft, error) {
	tenantID = strings.TrimSpace(tenantID)
	playbookID = strings.TrimSpace(playbookID)
	if tenantID == "" {
		return nil, errors.New("tenant_id is required")
	}
	if !playbookIDRe.MatchString(playbookID) {
		return nil, fmt.Errorf("invalid playbook_id %q (must match ^[a-z][a-z0-9-]*$)", playbookID)
	}
	if strings.TrimSpace(rawMarkdown) == "" {
		return nil, errors.New("raw_markdown is required")
	}

	draft := &Draft{
		TenantID:    tenantID,
		PlaybookID:  playbookID,
		RawMarkdown: rawMarkdown,
		UpdatedAt:   time.Now().UTC(),
	}
	_, err := s.db.NewInsert().
		Model(draft).
		On("CONFLICT (tenant_id, playbook_id) DO UPDATE").
		Set("raw_markdown = EXCLUDED.raw_markdown").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("saving draft %s/%s: %w", tenantID, playbookID, err)
	}
	return draft, nil
}

func (s *Store) GetDraft(ctx context.Context, tenantID, playbookID string) (*Draft, error) {
	draft := new(Draft)
	err := s.db.NewSelect().
		Model(draft).
		Where("tenant_id = ? AND playbook_id = ?", tenantID, playbookID).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s/%s", ErrDraftNotFound, tenantID, playbookID)
		}
		return nil, fmt.Errorf("getting draft %s/%s: %w", tenantID, playbookID, err)
	}
	return draft, nil
}

func (s *Store) DeleteDraft(ctx context.Context, tenantID, playbookID string) error {
	res, err := s.db.NewDelete().
		Model((*Draft)(nil)).
		Where("tenant_id = ? AND playbook_id = ?", tenantID, playbookID).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("deleting draft %s/%s: %w", tenantID, playbookID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: %s/%s", ErrDraftNotFound, tenantID, playbookID)
	}
	return nil
}

// Publish validates rawMarkdown and appends the next immutable version (MAX(version) + 1)
// into playbooks inside a serialized transaction.
func (s *Store) Publish(ctx context.Context, tenantID, rawMarkdown string) (*Record, error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return nil, errors.New("tenant_id is required")
	}

	pb, err := ParseMarkdown([]byte(rawMarkdown))
	if err != nil {
		return nil, err
	}

	var rec *Record
	err = s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`SELECT pg_advisory_xact_lock(hashtext(?), hashtext(?))`,
			tenantID, pb.PlaybookID,
		); err != nil {
			return fmt.Errorf("acquiring publish lock for %s/%s: %w", tenantID, pb.PlaybookID, err)
		}

		var maxVersion int
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(version), 0) FROM playbooks WHERE tenant_id = ? AND playbook_id = ?`,
			tenantID, pb.PlaybookID,
		).Scan(&maxVersion); err != nil {
			return fmt.Errorf("reading latest version for %s/%s: %w", tenantID, pb.PlaybookID, err)
		}

		pb.Version = maxVersion + 1
		canonicalMD, err := pb.RenderMarkdown()
		if err != nil {
			return err
		}
		hash, err := pb.ContentHash()
		if err != nil {
			return err
		}

		rec = &Record{
			TenantID:    tenantID,
			PlaybookID:  pb.PlaybookID,
			Version:     pb.Version,
			Name:        pb.Name,
			Description: pb.Description,
			Meta:        pb.Meta,
			RawMarkdown: string(canonicalMD),
			ContentHash: hash,
			CreatedAt:   time.Now().UTC(),
		}
		if _, err := tx.NewInsert().Model(rec).Exec(ctx); err != nil {
			return fmt.Errorf("inserting playbook %s/%s v%d: %w", tenantID, pb.PlaybookID, pb.Version, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// PublishDraft validates the current working draft for (tenantID, playbookID) and
// publishes it as the next immutable version.
func (s *Store) PublishDraft(ctx context.Context, tenantID, playbookID string) (*Record, error) {
	draft, err := s.GetDraft(ctx, tenantID, playbookID)
	if err != nil {
		return nil, err
	}
	pb, err := ParseMarkdown([]byte(draft.RawMarkdown))
	if err != nil {
		return nil, err
	}
	if pb.PlaybookID != playbookID {
		return nil, fmt.Errorf("draft frontmatter playbook_id %q does not match %q", pb.PlaybookID, playbookID)
	}
	return s.Publish(ctx, tenantID, draft.RawMarkdown)
}

func (s *Store) GetVersion(ctx context.Context, tenantID, playbookID string, version int) (*Record, error) {
	rec := new(Record)
	err := s.db.NewSelect().
		Model(rec).
		Where("tenant_id = ? AND playbook_id = ? AND version = ?", tenantID, playbookID, version).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s/%s v%d", ErrPlaybookNotFound, tenantID, playbookID, version)
		}
		return nil, fmt.Errorf("getting playbook %s/%s v%d: %w", tenantID, playbookID, version, err)
	}
	return rec, nil
}

func (s *Store) GetLatest(ctx context.Context, tenantID, playbookID string) (*Record, error) {
	rec := new(Record)
	err := s.db.NewSelect().
		Model(rec).
		Where("tenant_id = ? AND playbook_id = ?", tenantID, playbookID).
		OrderExpr("version DESC").
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s/%s", ErrPlaybookNotFound, tenantID, playbookID)
		}
		return nil, fmt.Errorf("getting latest playbook %s/%s: %w", tenantID, playbookID, err)
	}
	return rec, nil
}

func (s *Store) ListVersions(ctx context.Context, tenantID, playbookID string) ([]Record, error) {
	var records []Record
	err := s.db.NewSelect().
		Model(&records).
		Where("tenant_id = ? AND playbook_id = ?", tenantID, playbookID).
		OrderExpr("version DESC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing playbook versions for %s/%s: %w", tenantID, playbookID, err)
	}
	return records, nil
}
