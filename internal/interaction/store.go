package interaction

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/spacelions/eunomia/internal/actors"
)

// Task represents a durable human-in-the-loop question or L1 approval request tied to a CaseRun.
type Task struct {
	bun.BaseModel `bun:"table:interaction_tasks,alias:it"`

	TenantID        string         `bun:"tenant_id,pk" json:"tenant_id"`
	TaskID          string         `bun:"task_id,pk" json:"task_id"`
	RunID           string         `bun:"run_id,notnull" json:"run_id"`
	Kind            Kind           `bun:"kind,notnull" json:"kind"`
	Status          Status         `bun:"status,notnull" json:"status"`
	RecipientUserID string         `bun:"recipient_user_id,notnull" json:"recipient_user_id,omitempty"`
	RecipientEmail  string         `bun:"recipient_email,notnull" json:"recipient_email"`
	TokenHash       string         `bun:"token_hash,notnull" json:"-"`
	PromptPayload   map[string]any `bun:"prompt_payload,type:jsonb,notnull" json:"prompt_payload"`
	ResponsePayload map[string]any `bun:"response_payload,type:jsonb,notnull" json:"response_payload"`
	DeliveryMeta    map[string]any `bun:"delivery_meta,type:jsonb,notnull" json:"delivery_meta"`
	RespondedBy     string         `bun:"responded_by,notnull" json:"responded_by,omitempty"`
	ExpiresAt       time.Time      `bun:"expires_at,notnull" json:"expires_at"`
	RespondedAt     *time.Time     `bun:"responded_at,nullzero" json:"responded_at,omitempty"`
	CreatedAt       time.Time      `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt       time.Time      `bun:"updated_at,notnull" json:"updated_at"`
}

// CreateParams holds the inputs for opening a new interaction task.
type CreateParams struct {
	TenantID        string
	TaskID          string // optional; defaults to NewTaskID()
	RunID           string
	Kind            Kind
	RecipientUserID string
	RecipientEmail  string
	PromptPayload   map[string]any
	DeliveryMeta    map[string]any
	ExpiresAt       time.Time
}

// Store persists and atomically resolves InteractionTasks in PostgreSQL.
type Store struct {
	db *bun.DB
}

func NewStore(db *bun.DB) *Store {
	return &Store{db: db}
}

// Create opens a new interaction task and returns both the persisted Task and the one-time
// plaintext Magic Link token (only its SHA-256 hash is persisted).
func (s *Store) Create(ctx context.Context, p CreateParams) (*Task, string, error) {
	tenantID := strings.TrimSpace(p.TenantID)
	runID := strings.TrimSpace(p.RunID)
	if tenantID == "" {
		return nil, "", errors.New("tenant_id is required")
	}
	if runID == "" {
		return nil, "", errors.New("run_id is required")
	}
	if !p.Kind.IsValid() {
		return nil, "", fmt.Errorf("invalid interaction kind %q", p.Kind)
	}
	email, err := actors.NormalizeEmail(p.RecipientEmail)
	if err != nil {
		return nil, "", err
	}
	if p.ExpiresAt.IsZero() {
		return nil, "", errors.New("expires_at is required")
	}

	taskID := strings.TrimSpace(p.TaskID)
	if taskID == "" {
		taskID = NewTaskID()
	}

	plaintext, tokenHash, err := GenerateToken()
	if err != nil {
		return nil, "", fmt.Errorf("generating magic link token: %w", err)
	}

	prompt := p.PromptPayload
	if prompt == nil {
		prompt = map[string]any{}
	}
	delivery := p.DeliveryMeta
	if delivery == nil {
		delivery = map[string]any{}
	}

	now := time.Now().UTC()
	task := &Task{
		TenantID:        tenantID,
		TaskID:          taskID,
		RunID:           runID,
		Kind:            p.Kind,
		Status:          StatusOpen,
		RecipientUserID: strings.TrimSpace(p.RecipientUserID),
		RecipientEmail:  email,
		TokenHash:       tokenHash,
		PromptPayload:   prompt,
		ResponsePayload: map[string]any{},
		DeliveryMeta:    delivery,
		ExpiresAt:       p.ExpiresAt.UTC(),
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	if _, err := s.db.NewInsert().Model(task).Exec(ctx); err != nil {
		return nil, "", fmt.Errorf("creating interaction task %s/%s: %w", tenantID, taskID, err)
	}
	return task, plaintext, nil
}

func (s *Store) Get(ctx context.Context, tenantID, taskID string) (*Task, error) {
	task := new(Task)
	err := s.db.NewSelect().
		Model(task).
		Where("tenant_id = ? AND task_id = ?", tenantID, taskID).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s/%s", ErrTaskNotFound, tenantID, taskID)
		}
		return nil, fmt.Errorf("getting interaction task %s/%s: %w", tenantID, taskID, err)
	}
	return task, nil
}

// GetByToken resolves a plaintext Magic Link token by hashing it and querying the unique token_hash index.
func (s *Store) GetByToken(ctx context.Context, plaintextToken string) (*Task, error) {
	if strings.TrimSpace(plaintextToken) == "" {
		return nil, fmt.Errorf("%w: empty token", ErrTaskNotFound)
	}
	task := new(Task)
	err := s.db.NewSelect().
		Model(task).
		Where("token_hash = ?", HashToken(plaintextToken)).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrTaskNotFound
		}
		return nil, fmt.Errorf("getting interaction task by token: %w", err)
	}
	return task, nil
}

func (s *Store) ListByRun(ctx context.Context, tenantID, runID string) ([]Task, error) {
	var tasks []Task
	err := s.db.NewSelect().
		Model(&tasks).
		Where("tenant_id = ? AND run_id = ?", tenantID, runID).
		OrderExpr("created_at ASC, task_id ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing interaction tasks for %s/%s: %w", tenantID, runID, err)
	}
	return tasks, nil
}

// UpdateDeliveryMeta stores connector-agnostic delivery receipts (e.g., message IDs, thread timestamps).
func (s *Store) UpdateDeliveryMeta(ctx context.Context, tenantID, taskID string, meta map[string]any) error {
	if meta == nil {
		meta = map[string]any{}
	}
	res, err := s.db.NewUpdate().
		Model((*Task)(nil)).
		Set("delivery_meta = ?", meta).
		Set("updated_at = ?", time.Now().UTC()).
		Where("tenant_id = ? AND task_id = ?", tenantID, taskID).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("updating delivery meta for %s/%s: %w", tenantID, taskID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: %s/%s", ErrTaskNotFound, tenantID, taskID)
	}
	return nil
}

// Transition atomically moves an open InteractionTask into a terminal status using
// `WHERE status = 'open'` (plus a kind guard for question vs approval outcomes).
// Guarantees single-winner resolution under concurrent clicks or expiry sweeps.
func (s *Store) Transition(ctx context.Context, tenantID, taskID string, to Status, respondedBy string, responsePayload map[string]any) (*Task, error) {
	if !to.IsTerminal() {
		return nil, fmt.Errorf("%w: target status %q is not terminal", ErrInvalidTransition, to)
	}

	now := time.Now().UTC()
	q := s.db.NewUpdate().
		Model((*Task)(nil)).
		Set("status = ?", to).
		Set("responded_by = ?", strings.TrimSpace(respondedBy)).
		Set("updated_at = ?", now).
		Where("tenant_id = ? AND task_id = ? AND status = ?", tenantID, taskID, StatusOpen)

	if to == StatusSubmitted || to == StatusApproved || to == StatusRejected {
		q = q.Set("responded_at = ?", now)
	}
	if responsePayload != nil {
		q = q.Set("response_payload = ?", responsePayload)
	}

	switch to {
	case StatusSubmitted:
		q = q.Where("kind = ?", KindQuestion)
	case StatusApproved, StatusRejected:
		q = q.Where("kind = ?", KindApproval)
	}

	updated := new(Task)
	err := q.Returning("*").Scan(ctx, updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			current, getErr := s.Get(ctx, tenantID, taskID)
			if getErr != nil {
				return nil, getErr
			}
			return nil, fmt.Errorf("%w: task %s (kind=%q, status=%q) cannot transition to %q",
				ErrInvalidTransition, taskID, current.Kind, current.Status, to)
		}
		return nil, fmt.Errorf("transitioning interaction task %s/%s -> %s: %w", tenantID, taskID, to, err)
	}
	return updated, nil
}
