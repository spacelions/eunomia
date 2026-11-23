package actors

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"slices"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

// ErrActorNotFound is returned when an actor does not exist in the directory.
var ErrActorNotFound = errors.New("actor not found")

// Actor represents a real participant in a tenant's directory (or a verified external email).
type Actor struct {
	bun.BaseModel `bun:"table:actors,alias:a"`

	TenantID  string            `bun:"tenant_id,pk" json:"tenant_id"`
	UserID    string            `bun:"user_id,pk" json:"user_id"`
	Name      string            `bun:"name,notnull" json:"name"`
	Email     string            `bun:"email,notnull" json:"email"`
	Roles     []string          `bun:"roles,type:jsonb,notnull" json:"roles"`
	Handles   map[string]string `bun:"-" json:"handles,omitempty"`
	CreatedAt time.Time         `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time         `bun:"updated_at,notnull" json:"updated_at"`

	// External marks a transient participant outside the internal directory (e.g., new hire's personal_email).
	External bool `bun:"-" json:"external,omitempty"`
}

// Handle returns the actor's external identifier for provider (e.g. "slack", "notion", "linear").
func (a *Actor) Handle(provider string) string {
	if a == nil || len(a.Handles) == 0 {
		return ""
	}
	return a.Handles[NormalizeProvider(provider)]
}

type actorIdentity struct {
	bun.BaseModel `bun:"table:actor_identities,alias:ai"`

	TenantID   string    `bun:"tenant_id,pk"`
	UserID     string    `bun:"user_id,pk"`
	Provider   string    `bun:"provider,pk"`
	ExternalID string    `bun:"external_id,notnull"`
	CreatedAt  time.Time `bun:"created_at,notnull"`
	UpdatedAt  time.Time `bun:"updated_at,notnull"`
}

// Store persists and queries tenant-scoped actors and provider identities in PostgreSQL.
type Store struct {
	db *bun.DB
}

func NewStore(db *bun.DB) *Store {
	return &Store{db: db}
}

// NormalizeEmail validates an email address and returns its lowercase canonical form.
func NormalizeEmail(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("email is required")
	}
	addr, err := mail.ParseAddress(raw)
	if err != nil || addr.Address != raw || !strings.Contains(addr.Address, "@") {
		return "", fmt.Errorf("invalid email address %q", raw)
	}
	return strings.ToLower(addr.Address), nil
}

// NormalizeRole normalizes human-entered role labels (e.g. "IT Ops" or "it-ops" -> "it_ops").
func NormalizeRole(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.Join(strings.Fields(s), "_")
	return s
}

// NormalizeProvider normalizes external identity provider names (e.g. "Slack" or "google-workspace" -> "google_workspace").
func NormalizeProvider(raw string) string {
	return NormalizeRole(raw)
}

func normalizeRoles(roles []string) []string {
	seen := make(map[string]struct{}, len(roles))
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		nr := NormalizeRole(r)
		if nr == "" {
			continue
		}
		if _, dup := seen[nr]; dup {
			continue
		}
		seen[nr] = struct{}{}
		out = append(out, nr)
	}
	slices.Sort(out)
	return out
}

func normalizeHandles(raw map[string]string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	out := make(map[string]string, len(raw))
	for _, k := range keys {
		provider := NormalizeProvider(k)
		if provider == "" {
			return nil, errors.New("handle provider is required")
		}
		externalID := strings.TrimSpace(raw[k])
		if externalID == "" {
			return nil, fmt.Errorf("handle external_id for provider %q is required", provider)
		}
		if prev, exists := out[provider]; exists && prev != externalID {
			return nil, fmt.Errorf("conflicting handles for provider %q", provider)
		}
		out[provider] = externalID
	}
	return out, nil
}

func (s *Store) Upsert(ctx context.Context, actor Actor) error {
	actor.TenantID = strings.TrimSpace(actor.TenantID)
	actor.UserID = strings.TrimSpace(actor.UserID)
	actor.Name = strings.TrimSpace(actor.Name)

	if actor.TenantID == "" {
		return errors.New("tenant_id is required")
	}
	if actor.UserID == "" {
		return errors.New("user_id is required")
	}
	if actor.Name == "" {
		return errors.New("name is required")
	}
	email, err := NormalizeEmail(actor.Email)
	if err != nil {
		return err
	}
	handles, err := normalizeHandles(actor.Handles)
	if err != nil {
		return err
	}
	actor.Email = email
	actor.Roles = normalizeRoles(actor.Roles)
	actor.Handles = handles

	now := time.Now().UTC()
	actor.CreatedAt = now
	actor.UpdatedAt = now

	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		_, err := tx.NewInsert().
			Model(&actor).
			On("CONFLICT (tenant_id, user_id) DO UPDATE").
			Set("name = EXCLUDED.name").
			Set("email = EXCLUDED.email").
			Set("roles = EXCLUDED.roles").
			Set("updated_at = EXCLUDED.updated_at").
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("upserting actor %s/%s: %w", actor.TenantID, actor.UserID, err)
		}

		_, err = tx.NewDelete().
			Model((*actorIdentity)(nil)).
			Where("tenant_id = ? AND user_id = ?", actor.TenantID, actor.UserID).
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("clearing handles for actor %s/%s: %w", actor.TenantID, actor.UserID, err)
		}

		if len(handles) == 0 {
			return nil
		}

		providers := make([]string, 0, len(handles))
		for p := range handles {
			providers = append(providers, p)
		}
		slices.Sort(providers)

		rows := make([]actorIdentity, 0, len(providers))
		for _, p := range providers {
			rows = append(rows, actorIdentity{
				TenantID:   actor.TenantID,
				UserID:     actor.UserID,
				Provider:   p,
				ExternalID: handles[p],
				CreatedAt:  now,
				UpdatedAt:  now,
			})
		}
		if _, err := tx.NewInsert().Model(&rows).Exec(ctx); err != nil {
			return fmt.Errorf("saving handles for actor %s/%s: %w", actor.TenantID, actor.UserID, err)
		}
		return nil
	})
}

func (s *Store) Get(ctx context.Context, tenantID, userID string) (*Actor, error) {
	actor := new(Actor)
	err := s.db.NewSelect().
		Model(actor).
		Where("tenant_id = ? AND user_id = ?", tenantID, userID).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s/%s", ErrActorNotFound, tenantID, userID)
		}
		return nil, fmt.Errorf("getting actor %s/%s: %w", tenantID, userID, err)
	}
	if err := s.hydrateHandles(ctx, tenantID, []*Actor{actor}); err != nil {
		return nil, err
	}
	return actor, nil
}

func (s *Store) GetByEmail(ctx context.Context, tenantID, email string) (*Actor, error) {
	norm, err := NormalizeEmail(email)
	if err != nil {
		return nil, err
	}
	actor := new(Actor)
	err = s.db.NewSelect().
		Model(actor).
		Where("tenant_id = ? AND LOWER(email) = ?", tenantID, norm).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s/%s", ErrActorNotFound, tenantID, norm)
		}
		return nil, fmt.Errorf("getting actor by email %s/%s: %w", tenantID, norm, err)
	}
	if err := s.hydrateHandles(ctx, tenantID, []*Actor{actor}); err != nil {
		return nil, err
	}
	return actor, nil
}

// GetByHandle performs a reverse lookup from an external connector user ID (e.g. Slack webhook user_id)
// back to the tenant's Actor record.
func (s *Store) GetByHandle(ctx context.Context, tenantID, provider, externalID string) (*Actor, error) {
	prov := NormalizeProvider(provider)
	extID := strings.TrimSpace(externalID)
	if prov == "" || extID == "" {
		return nil, errors.New("provider and external_id are required")
	}

	var id actorIdentity
	err := s.db.NewSelect().
		Model(&id).
		Where("tenant_id = ? AND provider = ? AND external_id = ?", tenantID, prov, extID).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s/%s:%s", ErrActorNotFound, tenantID, prov, extID)
		}
		return nil, fmt.Errorf("getting actor by handle %s/%s:%s: %w", tenantID, prov, extID, err)
	}
	return s.Get(ctx, tenantID, id.UserID)
}

func (s *Store) List(ctx context.Context, tenantID string) ([]Actor, error) {
	var actors []Actor
	err := s.db.NewSelect().
		Model(&actors).
		Where("tenant_id = ?", tenantID).
		OrderExpr("user_id ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing actors for tenant %q: %w", tenantID, err)
	}
	if err := s.hydrateSlice(ctx, tenantID, actors); err != nil {
		return nil, err
	}
	return actors, nil
}

func (s *Store) Delete(ctx context.Context, tenantID, userID string) error {
	res, err := s.db.NewDelete().
		Model((*Actor)(nil)).
		Where("tenant_id = ? AND user_id = ?", tenantID, userID).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("deleting actor %s/%s: %w", tenantID, userID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: %s/%s", ErrActorNotFound, tenantID, userID)
	}
	return nil
}

// LookupPeople is the runtime directory search backing the agent's `lookup_people` tool.
// Matches actors in tenantID by normalized role (e.g. "IT Ops" -> "it_ops"), exact user_id,
// exact email, or case-insensitive substring on name/email.
func (s *Store) LookupPeople(ctx context.Context, tenantID, query string) ([]Actor, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, errors.New("lookup query is required")
	}

	roleJSON, _ := json.Marshal([]string{NormalizeRole(q)})
	likePattern := "%" + strings.ToLower(q) + "%"

	var matches []Actor
	err := s.db.NewSelect().
		Model(&matches).
		Where("tenant_id = ?", tenantID).
		WhereGroup(" AND ", func(sq *bun.SelectQuery) *bun.SelectQuery {
			return sq.
				Where("roles @> ?::jsonb", string(roleJSON)).
				WhereOr("LOWER(user_id) = ?", strings.ToLower(q)).
				WhereOr("LOWER(email) LIKE ?", likePattern).
				WhereOr("LOWER(name) LIKE ?", likePattern)
		}).
		OrderExpr("user_id ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("looking up people for %s/%q: %w", tenantID, query, err)
	}
	if err := s.hydrateSlice(ctx, tenantID, matches); err != nil {
		return nil, err
	}
	return matches, nil
}

func (s *Store) hydrateSlice(ctx context.Context, tenantID string, list []Actor) error {
	if len(list) == 0 {
		return nil
	}
	ptrs := make([]*Actor, len(list))
	for i := range list {
		ptrs[i] = &list[i]
	}
	return s.hydrateHandles(ctx, tenantID, ptrs)
}

func (s *Store) hydrateHandles(ctx context.Context, tenantID string, list []*Actor) error {
	if len(list) == 0 {
		return nil
	}
	byUser := make(map[string]*Actor, len(list))
	userIDs := make([]string, 0, len(list))
	for _, a := range list {
		byUser[a.UserID] = a
		userIDs = append(userIDs, a.UserID)
	}

	var rows []actorIdentity
	err := s.db.NewSelect().
		Model(&rows).
		Where("tenant_id = ? AND user_id IN (?)", tenantID, bun.In(userIDs)).
		OrderExpr("provider ASC").
		Scan(ctx)
	if err != nil {
		return fmt.Errorf("loading handles for tenant %q: %w", tenantID, err)
	}

	for _, r := range rows {
		a := byUser[r.UserID]
		if a.Handles == nil {
			a.Handles = make(map[string]string)
		}
		a.Handles[r.Provider] = r.ExternalID
	}
	return nil
}
