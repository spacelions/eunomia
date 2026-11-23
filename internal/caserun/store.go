package caserun

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

// CaseRun represents a single execution of a pinned immutable playbook version.
type CaseRun struct {
	bun.BaseModel `bun:"table:case_runs,alias:cr"`

	TenantID           string         `bun:"tenant_id,pk" json:"tenant_id"`
	RunID              string         `bun:"run_id,pk" json:"run_id"`
	PlaybookID         string         `bun:"playbook_id,notnull" json:"playbook_id"`
	PlaybookVersion    int            `bun:"playbook_version,notnull" json:"playbook_version"`
	TemporalWorkflowID string         `bun:"temporal_workflow_id,notnull" json:"temporal_workflow_id,omitempty"`
	Status             Status         `bun:"status,notnull" json:"status"`
	DryRun             bool           `bun:"dry_run,notnull" json:"dry_run"`
	Inputs             map[string]any `bun:"inputs,type:jsonb,notnull" json:"inputs"`
	CompletedTools     []string       `bun:"completed_tools,type:jsonb,notnull" json:"completed_tools"`
	TurnCount          int            `bun:"turn_count,notnull" json:"turn_count"`
	Result             map[string]any `bun:"result,type:jsonb,notnull" json:"result"`
	ErrorMessage       string         `bun:"error_message,notnull" json:"error_message,omitempty"`
	CreatedAt          time.Time      `bun:"created_at,notnull" json:"created_at"`
	StartedAt          *time.Time     `bun:"started_at,nullzero" json:"started_at,omitempty"`
	CompletedAt        *time.Time     `bun:"completed_at,nullzero" json:"completed_at,omitempty"`
	UpdatedAt          time.Time      `bun:"updated_at,notnull" json:"updated_at"`
}

// CreateParams holds the inputs for creating a new CaseRun in StatusPending.
type CreateParams struct {
	TenantID           string
	RunID              string // optional; defaults to NewRunID()
	PlaybookID         string
	PlaybookVersion    int
	TemporalWorkflowID string
	DryRun             bool
	Inputs             map[string]any
}

// ListFilter narrows CaseRun listings within a tenant.
type ListFilter struct {
	PlaybookID string
	Status     Status
}

// TransitionUpdate holds optional field updates applied atomically with a status transition.
type TransitionUpdate struct {
	TemporalWorkflowID *string
	Result             map[string]any
	ErrorMessage       *string
}

// Store persists and transitions CaseRuns in PostgreSQL.
type Store struct {
	db *bun.DB
}

func NewStore(db *bun.DB) *Store {
	return &Store{db: db}
}

func (s *Store) Create(ctx context.Context, p CreateParams) (*CaseRun, error) {
	tenantID := strings.TrimSpace(p.TenantID)
	playbookID := strings.TrimSpace(p.PlaybookID)
	if tenantID == "" {
		return nil, errors.New("tenant_id is required")
	}
	if playbookID == "" {
		return nil, errors.New("playbook_id is required")
	}
	if p.PlaybookVersion < 1 {
		return nil, fmt.Errorf("playbook_version must be >= 1, got %d", p.PlaybookVersion)
	}

	runID := strings.TrimSpace(p.RunID)
	if runID == "" {
		runID = NewRunID()
	}

	inputs := p.Inputs
	if inputs == nil {
		inputs = map[string]any{}
	}

	now := time.Now().UTC()
	run := &CaseRun{
		TenantID:           tenantID,
		RunID:              runID,
		PlaybookID:         playbookID,
		PlaybookVersion:    p.PlaybookVersion,
		TemporalWorkflowID: strings.TrimSpace(p.TemporalWorkflowID),
		Status:             StatusPending,
		DryRun:             p.DryRun,
		Inputs:             inputs,
		CompletedTools:     []string{},
		Result:             map[string]any{},
		CreatedAt:          now,
		UpdatedAt:          now,
	}

	if _, err := s.db.NewInsert().Model(run).Exec(ctx); err != nil {
		return nil, fmt.Errorf("creating case run %s/%s: %w", tenantID, runID, err)
	}
	return run, nil
}

func (s *Store) Get(ctx context.Context, tenantID, runID string) (*CaseRun, error) {
	run := new(CaseRun)
	err := s.db.NewSelect().
		Model(run).
		Where("tenant_id = ? AND run_id = ?", tenantID, runID).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s/%s", ErrRunNotFound, tenantID, runID)
		}
		return nil, fmt.Errorf("getting case run %s/%s: %w", tenantID, runID, err)
	}
	return run, nil
}

func (s *Store) List(ctx context.Context, tenantID string, filter ListFilter) ([]CaseRun, error) {
	var runs []CaseRun
	q := s.db.NewSelect().
		Model(&runs).
		Where("tenant_id = ?", tenantID)
	if strings.TrimSpace(filter.PlaybookID) != "" {
		q = q.Where("playbook_id = ?", strings.TrimSpace(filter.PlaybookID))
	}
	if filter.Status != "" {
		if !filter.Status.IsValid() {
			return nil, fmt.Errorf("invalid status filter %q", filter.Status)
		}
		q = q.Where("status = ?", filter.Status)
	}
	if err := q.OrderExpr("created_at DESC, run_id DESC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing case runs for tenant %q: %w", tenantID, err)
	}
	return runs, nil
}

// Transition atomically moves a CaseRun from `from` -> `to` using an optimistic
// `WHERE status = from` guard. Returns ErrInvalidTransition if the edge is disallowed
// or another caller already transitioned the run out of `from`.
func (s *Store) Transition(ctx context.Context, tenantID, runID string, from, to Status, upd TransitionUpdate) (*CaseRun, error) {
	if !CanTransition(from, to) {
		return nil, fmt.Errorf("%w: %s -> %s is not allowed", ErrInvalidTransition, from, to)
	}

	now := time.Now().UTC()
	q := s.db.NewUpdate().
		Model((*CaseRun)(nil)).
		Set("status = ?", to).
		Set("updated_at = ?", now)

	if to == StatusRunning {
		q = q.Set("started_at = COALESCE(started_at, ?)", now)
	}
	if to.IsTerminal() {
		q = q.Set("completed_at = ?", now)
	}
	if upd.TemporalWorkflowID != nil {
		q = q.Set("temporal_workflow_id = ?", strings.TrimSpace(*upd.TemporalWorkflowID))
	}
	if upd.Result != nil {
		q = q.Set("result = ?", upd.Result)
	}
	if upd.ErrorMessage != nil {
		q = q.Set("error_message = ?", *upd.ErrorMessage)
	}

	updated := new(CaseRun)
	err := q.
		Where("tenant_id = ? AND run_id = ? AND status = ?", tenantID, runID, from).
		Returning("*").
		Scan(ctx, updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			current, getErr := s.Get(ctx, tenantID, runID)
			if getErr != nil {
				return nil, getErr
			}
			return nil, fmt.Errorf("%w: run %s is in status %q, expected %q", ErrInvalidTransition, runID, current.Status, from)
		}
		return nil, fmt.Errorf("transitioning case run %s/%s (%s -> %s): %w", tenantID, runID, from, to, err)
	}
	return updated, nil
}

// RecordToolCompletion records a completed connector tool (deduped, sorted) and optionally
// increments turn_count inside a transaction.
func (s *Store) RecordToolCompletion(ctx context.Context, tenantID, runID, toolName string, incrementTurn bool) (*CaseRun, error) {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return nil, errors.New("tool_name is required")
	}

	var updated *CaseRun
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		run := new(CaseRun)
		err := tx.NewSelect().
			Model(run).
			Where("tenant_id = ? AND run_id = ?", tenantID, runID).
			For("UPDATE").
			Scan(ctx)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: %s/%s", ErrRunNotFound, tenantID, runID)
			}
			return fmt.Errorf("locking case run %s/%s: %w", tenantID, runID, err)
		}

		if !slices.Contains(run.CompletedTools, toolName) {
			run.CompletedTools = append(run.CompletedTools, toolName)
			slices.Sort(run.CompletedTools)
		}
		if incrementTurn {
			run.TurnCount++
		}
		run.UpdatedAt = time.Now().UTC()

		if _, err := tx.NewUpdate().
			Model(run).
			Column("completed_tools", "turn_count", "updated_at").
			WherePK().
			Exec(ctx); err != nil {
			return fmt.Errorf("recording tool completion for %s/%s: %w", tenantID, runID, err)
		}
		updated = run
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}
