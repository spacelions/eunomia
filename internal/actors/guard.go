package actors

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

var (
	// ErrRecipientNotAllowed is returned when an agent attempts to contact an address
	// that is neither in the tenant's actors directory nor explicitly authorized for this run.
	ErrRecipientNotAllowed = errors.New("recipient not allowed")

	// ErrMissingProviderHandle is returned when a verified actor lacks an external identity
	// for the requested connector provider in actor_identities.
	ErrMissingProviderHandle = errors.New("actor missing required provider handle")
)

// Directory abstracts the read-only actor lookups needed by RecipientGuard.
type Directory interface {
	Get(ctx context.Context, tenantID, userID string) (*Actor, error)
	GetByEmail(ctx context.Context, tenantID, email string) (*Actor, error)
}

// RecipientGuard enforces the Recipient Allowlist Guardrail: outbound messages and
// Magic Links may only target verified actors from the tenant's directory OR external
// emails explicitly registered from the run's trigger inputs / prior connector outputs.
type RecipientGuard struct {
	dir      Directory
	tenantID string

	mu            sync.RWMutex
	allowedEmails map[string]struct{}
}

// NewRecipientGuard creates a per-run guard seeded with any trusted external emails
// (for example, a new hire's personal_email supplied at trigger time).
func NewRecipientGuard(dir Directory, tenantID string, initialExternalEmails ...string) (*RecipientGuard, error) {
	g := &RecipientGuard{
		dir:           dir,
		tenantID:      tenantID,
		allowedEmails: make(map[string]struct{}, len(initialExternalEmails)),
	}
	for _, raw := range initialExternalEmails {
		if err := g.AllowEmail(raw); err != nil {
			return nil, err
		}
	}
	return g, nil
}

// AllowEmail registers an external email discovered from a trusted source
// (e.g., trigger input or connector output such as a newly provisioned work account).
func (g *RecipientGuard) AllowEmail(raw string) error {
	norm, err := NormalizeEmail(raw)
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.allowedEmails[norm] = struct{}{}
	g.mu.Unlock()
	return nil
}

// Authorize resolves target (user_id or email) against the tenant's directory or the run's
// external email allowlist and returns the verified Actor (with a canonical Email).
func (g *RecipientGuard) Authorize(ctx context.Context, target string) (*Actor, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, fmt.Errorf("%w: target is empty", ErrRecipientNotAllowed)
	}

	if strings.Contains(target, "@") {
		norm, normErr := NormalizeEmail(target)
		if normErr != nil {
			return nil, fmt.Errorf("%w: %v", ErrRecipientNotAllowed, normErr)
		}
		found, err := g.dir.GetByEmail(ctx, g.tenantID, norm)
		if err == nil {
			return found, nil
		}
		if !errors.Is(err, ErrActorNotFound) {
			return nil, err
		}

		g.mu.RLock()
		_, allowed := g.allowedEmails[norm]
		g.mu.RUnlock()
		if !allowed {
			return nil, fmt.Errorf("%w: %q is not in the actor directory or run allowlist", ErrRecipientNotAllowed, norm)
		}
		return &Actor{
			TenantID: g.tenantID,
			Email:    norm,
			External: true,
		}, nil
	}

	found, err := g.dir.Get(ctx, g.tenantID, target)
	if err != nil {
		if errors.Is(err, ErrActorNotFound) {
			return nil, fmt.Errorf("%w: user_id %q not found in directory", ErrRecipientNotAllowed, target)
		}
		return nil, err
	}
	return found, nil
}

// AuthorizeHandle authorizes target and verifies the resolved actor has an external
// identity handle for provider (e.g. "slack", "notion", "linear"), returning both
// the Actor and the provider's external ID.
func (g *RecipientGuard) AuthorizeHandle(ctx context.Context, target, provider string) (*Actor, string, error) {
	prov := NormalizeProvider(provider)
	if prov == "" {
		return nil, "", errors.New("provider is required")
	}

	actor, err := g.Authorize(ctx, target)
	if err != nil {
		return nil, "", err
	}

	handle := actor.Handle(prov)
	if handle == "" {
		return nil, "", fmt.Errorf("%w: %q has no %q handle", ErrMissingProviderHandle, actor.Email, prov)
	}
	return actor, handle, nil
}
