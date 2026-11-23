package actors

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

const (
	ChannelEmail   = "email"
	ChannelGmail   = "gmail"
	ChannelSlack   = "slack"
	ChannelSlackDM = "slack_dm"
	ChannelNotion  = "notion"
)

var (
	// ErrRecipientNotAllowed is returned when an agent attempts to contact an address
	// that is neither in the tenant's actors directory nor explicitly authorized for this run.
	ErrRecipientNotAllowed = errors.New("recipient not allowed")

	// ErrMissingChannelHandle is returned when a verified actor lacks the contact ID
	// required for the requested channel (e.g., a "slack" or "linear" handle in actor_identities).
	ErrMissingChannelHandle = errors.New("actor missing required channel handle")
)

// Directory abstracts the read-only actor lookups needed by RecipientGuard.
type Directory interface {
	Get(ctx context.Context, tenantID, userID string) (*Actor, error)
	GetByEmail(ctx context.Context, tenantID, email string) (*Actor, error)
}

// RecipientGuard enforces the Recipient Allowlist Guardrail: outbound emails and
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
// (e.g., trigger input or connector output such as google_workspace.create_user).
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
// external email allowlist and verifies the resolved actor supports the requested channel.
//
// Channel checks are provider-agnostic: "email"/"gmail" require Actor.Email, "slack_dm"
// aliases to provider "slack", and any other provider name (e.g. "notion", "linear", "github")
// requires a matching entry in Actor.Handles.
func (g *RecipientGuard) Authorize(ctx context.Context, target, channel string) (*Actor, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, fmt.Errorf("%w: target is empty", ErrRecipientNotAllowed)
	}

	var actor *Actor
	if strings.Contains(target, "@") {
		norm, normErr := NormalizeEmail(target)
		if normErr != nil {
			return nil, fmt.Errorf("%w: %v", ErrRecipientNotAllowed, normErr)
		}
		found, err := g.dir.GetByEmail(ctx, g.tenantID, norm)
		if err == nil {
			actor = found
		} else if !errors.Is(err, ErrActorNotFound) {
			return nil, err
		} else {
			g.mu.RLock()
			_, allowed := g.allowedEmails[norm]
			g.mu.RUnlock()
			if !allowed {
				return nil, fmt.Errorf("%w: %q is not in the actor directory or run allowlist", ErrRecipientNotAllowed, norm)
			}
			actor = &Actor{
				TenantID: g.tenantID,
				Email:    norm,
				External: true,
			}
		}
	} else {
		found, err := g.dir.Get(ctx, g.tenantID, target)
		if err != nil {
			if errors.Is(err, ErrActorNotFound) {
				return nil, fmt.Errorf("%w: user_id %q not found in directory", ErrRecipientNotAllowed, target)
			}
			return nil, err
		}
		actor = found
	}

	if err := checkChannelHandle(actor, channel); err != nil {
		return nil, err
	}
	return actor, nil
}

func checkChannelHandle(actor *Actor, channel string) error {
	ch := NormalizeProvider(channel)
	if ch == "" {
		return errors.New("channel is required")
	}
	if ch == ChannelEmail || ch == ChannelGmail {
		if actor.Email == "" {
			return fmt.Errorf("%w: %q has no email for channel %q", ErrMissingChannelHandle, actor.UserID, channel)
		}
		return nil
	}

	provider := ch
	if provider == ChannelSlackDM {
		provider = ChannelSlack
	}
	if actor.Handle(provider) == "" {
		return fmt.Errorf("%w: %q has no %q handle for channel %q", ErrMissingChannelHandle, actor.Email, provider, channel)
	}
	return nil
}
