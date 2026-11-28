package connector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

// ExecutionStatus tracks an idempotent connector call in Postgres.
type ExecutionStatus string

const (
	ExecutionInFlight  ExecutionStatus = "in_flight"
	ExecutionSucceeded ExecutionStatus = "succeeded"
	ExecutionFailed    ExecutionStatus = "failed"
)

// Execution is a single durable row in connector_executions keyed by (tenant_id, idempotency_key).
type Execution struct {
	bun.BaseModel `bun:"table:connector_executions,alias:ce"`

	TenantID       string          `bun:"tenant_id,pk" json:"tenant_id"`
	IdempotencyKey string          `bun:"idempotency_key,pk" json:"idempotency_key"`
	RunID          string          `bun:"run_id,notnull" json:"run_id,omitempty"`
	ConnectorType  string          `bun:"connector_type,notnull" json:"connector_type"`
	Operation      string          `bun:"operation,notnull" json:"operation"`
	RiskTier       RiskTier        `bun:"risk_tier,notnull" json:"risk_tier"`
	Status         ExecutionStatus `bun:"status,notnull" json:"status"`
	Response       map[string]any  `bun:"response,type:jsonb,notnull" json:"response"`
	ErrorMessage   string          `bun:"error_message,notnull" json:"error_message,omitempty"`
	CreatedAt      time.Time       `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt      time.Time       `bun:"updated_at,notnull" json:"updated_at"`
}

// ClaimParams identifies a mutating connector call entering the idempotency ledger.
type ClaimParams struct {
	TenantID       string
	IdempotencyKey string
	RunID          string
	ConnectorType  string
	Operation      string
	RiskTier       RiskTier
}

// Ledger persists mutating connector executions in Postgres to guarantee replay-safe idempotency
// across Temporal retries — even for external APIs without native idempotency keys.
type Ledger struct {
	db *bun.DB
}

func NewLedger(db *bun.DB) *Ledger {
	return &Ledger{db: db}
}

// Claim atomically reserves (tenant_id, idempotency_key) for execution:
//   - Fresh key -> inserts `in_flight` row and returns (nil, true, nil).
//   - Prior `succeeded` -> returns (cachedRow, false, nil) so caller replays cached output.
//   - Prior `in_flight` -> returns (nil, false, ErrExecutionInFlight).
//   - Prior `failed` -> reclaims row back to `in_flight` and returns (nil, true, nil) for clean retry.
func (l *Ledger) Claim(ctx context.Context, p ClaimParams) (*Execution, bool, error) {
	if strings.TrimSpace(p.TenantID) == "" {
		return nil, false, errors.New("tenant_id is required")
	}
	if strings.TrimSpace(p.IdempotencyKey) == "" {
		return nil, false, ErrIdempotencyKeyRequired
	}
	if !p.RiskTier.RequiresIdempotency() {
		return nil, false, fmt.Errorf("risk tier %q does not use the idempotency ledger", p.RiskTier)
	}

	var cached *Execution
	var claimed bool

	err := l.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		now := time.Now().UTC()
		row := &Execution{
			TenantID:       p.TenantID,
			IdempotencyKey: p.IdempotencyKey,
			RunID:          strings.TrimSpace(p.RunID),
			ConnectorType:  p.ConnectorType,
			Operation:      p.Operation,
			RiskTier:       p.RiskTier,
			Status:         ExecutionInFlight,
			Response:       map[string]any{},
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		res, err := tx.NewInsert().
			Model(row).
			On("CONFLICT (tenant_id, idempotency_key) DO NOTHING").
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("claiming execution %s/%s: %w", p.TenantID, p.IdempotencyKey, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 1 {
			claimed = true
			return nil
		}

		existing := new(Execution)
		if err := tx.NewSelect().
			Model(existing).
			Where("tenant_id = ? AND idempotency_key = ?", p.TenantID, p.IdempotencyKey).
			For("UPDATE").
			Scan(ctx); err != nil {
			return fmt.Errorf("locking existing execution %s/%s: %w", p.TenantID, p.IdempotencyKey, err)
		}

		if existing.ConnectorType != p.ConnectorType || existing.Operation != p.Operation {
			return fmt.Errorf("%w: %q belongs to %s.%s, not %s.%s",
				ErrIdempotencyKeyMismatch, p.IdempotencyKey, existing.ConnectorType, existing.Operation, p.ConnectorType, p.Operation)
		}

		switch existing.Status {
		case ExecutionSucceeded:
			cached = existing
			return nil
		case ExecutionInFlight:
			return fmt.Errorf("%w: %s/%s", ErrExecutionInFlight, p.TenantID, p.IdempotencyKey)
		case ExecutionFailed:
			if _, err := tx.NewUpdate().
				Model((*Execution)(nil)).
				Set("status = ?", ExecutionInFlight).
				Set("error_message = ?", "").
				Set("updated_at = ?", now).
				Where("tenant_id = ? AND idempotency_key = ?", p.TenantID, p.IdempotencyKey).
				Exec(ctx); err != nil {
				return fmt.Errorf("reclaiming failed execution %s/%s: %w", p.TenantID, p.IdempotencyKey, err)
			}
			claimed = true
			return nil
		default:
			return fmt.Errorf("unexpected execution status %q for %s/%s", existing.Status, p.TenantID, p.IdempotencyKey)
		}
	})
	if err != nil {
		return nil, false, err
	}
	return cached, claimed, nil
}

// MarkSucceeded records a completed connector call's output so subsequent retries replay it.
func (l *Ledger) MarkSucceeded(ctx context.Context, tenantID, idempotencyKey string, response map[string]any) error {
	if response == nil {
		response = map[string]any{}
	}
	res, err := l.db.NewUpdate().
		Model((*Execution)(nil)).
		Set("status = ?", ExecutionSucceeded).
		Set("response = ?", response).
		Set("error_message = ?", "").
		Set("updated_at = ?", time.Now().UTC()).
		Where("tenant_id = ? AND idempotency_key = ? AND status = ?", tenantID, idempotencyKey, ExecutionInFlight).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("marking execution succeeded %s/%s: %w", tenantID, idempotencyKey, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// MarkFailed records a failed connector call so a subsequent Temporal retry may reclaim it.
func (l *Ledger) MarkFailed(ctx context.Context, tenantID, idempotencyKey, errMsg string) error {
	res, err := l.db.NewUpdate().
		Model((*Execution)(nil)).
		Set("status = ?", ExecutionFailed).
		Set("error_message = ?", errMsg).
		Set("updated_at = ?", time.Now().UTC()).
		Where("tenant_id = ? AND idempotency_key = ? AND status = ?", tenantID, idempotencyKey, ExecutionInFlight).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("marking execution failed %s/%s: %w", tenantID, idempotencyKey, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListByRun returns the chronological execution audit trail for a CaseRun.
func (l *Ledger) ListByRun(ctx context.Context, tenantID, runID string) ([]Execution, error) {
	var rows []Execution
	err := l.db.NewSelect().
		Model(&rows).
		Where("tenant_id = ? AND run_id = ?", tenantID, runID).
		OrderExpr("created_at ASC, idempotency_key ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing connector executions for %s/%s: %w", tenantID, runID, err)
	}
	return rows, nil
}
