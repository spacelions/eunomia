package connector

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spacelions/eunomia/internal/connectorstore"
	"github.com/spacelions/eunomia/internal/secrets"
)

// ConfigStore reads per-tenant connector settings and resource aliases from PostgreSQL.
type ConfigStore interface {
	Get(ctx context.Context, tenantID, connectorType string) (*connectorstore.Record, error)
	ListEnabled(ctx context.Context, tenantID string) ([]connectorstore.Record, error)
}

// Registry manages connector factories, instantiates tenant-scoped connectors with isolated
// secrets, runs health checks, exports capability catalogs, and enforces idempotency + dry_run.
type Registry struct {
	configs        ConfigStore
	secretProvider secrets.SecretProvider
	ledger         *Ledger

	mu        sync.RWMutex
	factories map[string]Factory
}

func NewRegistry(configs ConfigStore, secretProvider secrets.SecretProvider, ledger *Ledger) *Registry {
	return &Registry{
		configs:        configs,
		secretProvider: secretProvider,
		ledger:         ledger,
		factories:      make(map[string]Factory),
	}
}

// Register binds a connector Factory to its canonical connector_type (e.g. "google_workspace", "notion").
func (r *Registry) Register(connectorType string, factory Factory) error {
	ct := strings.ToLower(strings.TrimSpace(connectorType))
	if ct == "" {
		return errors.New("connector type is required")
	}
	if factory == nil {
		return fmt.Errorf("factory for connector %q must not be nil", ct)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.factories[ct]; exists {
		return fmt.Errorf("connector %q is already registered", ct)
	}
	r.factories[ct] = factory
	return nil
}

// RegisteredTypes returns a sorted list of all registered connector types.
func (r *Registry) RegisteredTypes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.factories))
	for k := range r.factories {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Resolve loads tenant configuration for connectorType, verifies it is enabled, and constructs
// the connector with a SecretReader scoped strictly to (tenantID, connectorType).
func (r *Registry) Resolve(ctx context.Context, tenantID, connectorType string) (Connector, *TenantConfig, error) {
	ct := strings.ToLower(strings.TrimSpace(connectorType))
	factory, ok := r.factory(ct)
	if !ok {
		return nil, nil, fmt.Errorf("%w: %q", ErrUnknownConnector, ct)
	}

	rec, err := r.configs.Get(ctx, tenantID, ct)
	if err != nil {
		if errors.Is(err, connectorstore.ErrConnectorNotFound) {
			return nil, nil, fmt.Errorf("%w: %s/%s", ErrConnectorDisabled, tenantID, ct)
		}
		return nil, nil, err
	}
	if !rec.Enabled {
		return nil, nil, fmt.Errorf("%w: %s/%s", ErrConnectorDisabled, tenantID, ct)
	}

	return r.instantiate(ctx, factory, *rec)
}

// ResolveChannel resolves a tenant-enabled connector and verifies it implements Channel.
func (r *Registry) ResolveChannel(ctx context.Context, tenantID, connectorType string) (Channel, error) {
	conn, _, err := r.Resolve(ctx, tenantID, connectorType)
	if err != nil {
		return nil, err
	}
	ch, ok := conn.(Channel)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotAChannel, conn.Type())
	}
	return ch, nil
}

// CheckTenantHealth runs HealthCheck(ctx) on every enabled connector for tenantID and returns
// structured per-connector diagnostics without failing on the first unhealthy connector.
func (r *Registry) CheckTenantHealth(ctx context.Context, tenantID string) (*HealthReport, error) {
	recs, err := r.configs.ListEnabled(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	report := &HealthReport{
		TenantID:   tenantID,
		Connectors: make([]ConnectorHealth, 0, len(recs)),
	}
	for _, rec := range recs {
		ct := strings.ToLower(strings.TrimSpace(rec.ConnectorType))
		factory, ok := r.factory(ct)
		if !ok {
			report.Connectors = append(report.Connectors, ConnectorHealth{
				Type:    ct,
				Healthy: false,
				Error:   fmt.Sprintf("%v: %q", ErrUnknownConnector, ct),
			})
			continue
		}
		conn, _, err := r.instantiate(ctx, factory, rec)
		if err != nil {
			report.Connectors = append(report.Connectors, ConnectorHealth{
				Type:    ct,
				Healthy: false,
				Error:   err.Error(),
			})
			continue
		}
		if err := conn.HealthCheck(ctx); err != nil {
			report.Connectors = append(report.Connectors, ConnectorHealth{
				Type:    ct,
				Healthy: false,
				Error:   err.Error(),
			})
			continue
		}
		report.Connectors = append(report.Connectors, ConnectorHealth{
			Type:    ct,
			Healthy: true,
		})
	}
	return report, nil
}

// Catalog returns a read-only capability snapshot of all enabled connectors for tenantID
// (operations, risk tiers, channel capability, and semantic resource aliases).
func (r *Registry) Catalog(ctx context.Context, tenantID string) (*CatalogSnapshot, error) {
	recs, err := r.configs.ListEnabled(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	snap := &CatalogSnapshot{
		TenantID:   tenantID,
		Connectors: make([]ConnectorCatalog, 0, len(recs)),
	}
	for _, rec := range recs {
		ct := strings.ToLower(strings.TrimSpace(rec.ConnectorType))
		factory, ok := r.factory(ct)
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrUnknownConnector, ct)
		}
		conn, cfg, err := r.instantiate(ctx, factory, rec)
		if err != nil {
			return nil, err
		}
		_, isChannel := conn.(Channel)
		snap.Connectors = append(snap.Connectors, ConnectorCatalog{
			Type:       conn.Type(),
			IsChannel:  isChannel,
			Operations: slices.Clone(conn.Operations()),
			Resources:  maps.Clone(cfg.Resources),
		})
	}
	return snap, nil
}

// Execute dispatches a connector operation with Guardrails:
//   - Verifies connector is enabled for req.TenantID and exposes req.Operation.
//   - Injects tenant resource aliases into req.Resources.
//   - For RiskRead: calls connector directly (no idempotency ledger).
//   - For RiskWrite / RiskIrreversible: simulates when req.DryRun is true, otherwise enforces
//     idempotency via the shared Postgres Ledger (replay on succeeded, reject on in_flight).
func (r *Registry) Execute(ctx context.Context, connectorType string, req CallRequest) (*CallResult, error) {
	conn, cfg, err := r.Resolve(ctx, req.TenantID, connectorType)
	if err != nil {
		return nil, err
	}

	opSpec, ok := findOperation(conn.Operations(), req.Operation)
	if !ok {
		return nil, fmt.Errorf("%w: %s.%s", ErrUnknownOperation, conn.Type(), req.Operation)
	}
	if !opSpec.RiskTier.IsValid() {
		return nil, fmt.Errorf("invalid risk tier %q on %s.%s", opSpec.RiskTier, conn.Type(), opSpec.Name)
	}

	req.Resources = maps.Clone(cfg.Resources)
	if req.Params == nil {
		req.Params = map[string]any{}
	}

	if !opSpec.RiskTier.RequiresIdempotency() {
		return conn.Call(ctx, req)
	}

	if req.DryRun {
		return &CallResult{
			Simulated: true,
			Output: map[string]any{
				"dry_run":   true,
				"connector": conn.Type(),
				"operation": opSpec.Name,
				"risk_tier": string(opSpec.RiskTier),
			},
		}, nil
	}

	idemKey := strings.TrimSpace(req.IdempotencyKey)
	if idemKey == "" {
		idemKey, err = FormatIdempotencyKey(req.RunID, req.StepKey)
		if err != nil {
			return nil, err
		}
	}
	req.IdempotencyKey = idemKey

	if r.ledger != nil {
		cached, claimed, err := r.ledger.Claim(ctx, ClaimParams{
			TenantID:       req.TenantID,
			IdempotencyKey: idemKey,
			RunID:          req.RunID,
			ConnectorType:  conn.Type(),
			Operation:      opSpec.Name,
			RiskTier:       opSpec.RiskTier,
		})
		if err != nil {
			return nil, err
		}
		if !claimed {
			return &CallResult{
				Output:   maps.Clone(cached.Response),
				Replayed: true,
			}, nil
		}
	}

	res, callErr := conn.Call(ctx, req)
	if callErr != nil {
		if r.ledger != nil {
			finalizeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = r.ledger.MarkFailed(finalizeCtx, req.TenantID, idemKey, callErr.Error())
		}
		return nil, callErr
	}

	if res == nil {
		res = &CallResult{Output: map[string]any{}}
	}
	if res.Output == nil {
		res.Output = map[string]any{}
	}
	if r.ledger != nil {
		finalizeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := r.ledger.MarkSucceeded(finalizeCtx, req.TenantID, idemKey, res.Output); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func (r *Registry) factory(connectorType string) (Factory, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.factories[connectorType]
	return f, ok
}

func (r *Registry) instantiate(ctx context.Context, factory Factory, rec connectorstore.Record) (Connector, *TenantConfig, error) {
	ct := strings.ToLower(strings.TrimSpace(rec.ConnectorType))
	cfg := &TenantConfig{
		TenantID:      rec.TenantID,
		ConnectorType: ct,
		Settings:      maps.Clone(rec.Settings),
		Resources:     maps.Clone(rec.Resources),
	}
	scopedSecrets := &scopedSecretReader{
		provider:      r.secretProvider,
		tenantID:      rec.TenantID,
		connectorType: ct,
	}
	conn, err := factory(ctx, *cfg, scopedSecrets)
	if err != nil {
		return nil, nil, fmt.Errorf("initializing connector %s/%s: %w", rec.TenantID, ct, err)
	}
	return conn, cfg, nil
}

func findOperation(ops []OperationSpec, name string) (OperationSpec, bool) {
	for _, op := range ops {
		if op.Name == name {
			return op, true
		}
	}
	return OperationSpec{}, false
}
