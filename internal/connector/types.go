package connector

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spacelions/eunomia/internal/secrets"
)

// RiskTier classifies how a connector operation interacts with external systems
// and which Go guardrails (dry_run, idempotency ledger, L1 approval gate) apply.
type RiskTier string

const (
	// RiskRead is a side-effect-free query safe in dry_run and outside the idempotency ledger.
	RiskRead RiskTier = "read"

	// RiskWrite is an idempotent internal mutation simulated in dry_run and guarded by the ledger.
	RiskWrite RiskTier = "write"

	// RiskIrreversible is an external/human-visible or destructive action gated by L1 approval & ledger.
	RiskIrreversible RiskTier = "irreversible"
)

var (
	// ErrUnknownConnector is returned when no factory is registered for connector_type.
	ErrUnknownConnector = errors.New("unknown connector type")

	// ErrConnectorDisabled is returned when a tenant has not enabled the requested connector.
	ErrConnectorDisabled = errors.New("connector not enabled for tenant")

	// ErrUnknownOperation is returned when a connector does not expose the requested operation.
	ErrUnknownOperation = errors.New("unknown connector operation")

	// ErrNotAChannel is returned when a connector does not implement Channel.
	ErrNotAChannel = errors.New("connector does not support human interaction delivery")

	// ErrIdempotencyKeyRequired is returned when a write/irreversible call lacks an idempotency key.
	ErrIdempotencyKeyRequired = errors.New("idempotency_key (or run_id + step_key) is required for mutating operations")

	// ErrExecutionInFlight is returned when another worker is currently executing the same idempotency_key.
	ErrExecutionInFlight = errors.New("connector execution already in flight")
)

// IsValid reports whether r is a recognized RiskTier.
func (r RiskTier) IsValid() bool {
	return r == RiskRead || r == RiskWrite || r == RiskIrreversible
}

// RequiresIdempotency reports whether operations of tier r must pass through the idempotency ledger.
func (r RiskTier) RequiresIdempotency() bool {
	return r == RiskWrite || r == RiskIrreversible
}

// FormatIdempotencyKey builds the canonical `runID:stepKey` idempotency key.
func FormatIdempotencyKey(runID, stepKey string) (string, error) {
	runID = strings.TrimSpace(runID)
	stepKey = strings.TrimSpace(stepKey)
	if runID == "" || stepKey == "" {
		return "", ErrIdempotencyKeyRequired
	}
	return runID + ":" + stepKey, nil
}

// OperationSpec describes a single callable capability exposed by a Connector.
type OperationSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	RiskTier    RiskTier       `json:"risk_tier"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// TenantConfig carries non-secret per-tenant settings and semantic resource aliases
// loaded from tenant_connectors.
type TenantConfig struct {
	TenantID      string
	ConnectorType string
	Settings      map[string]string
	Resources     map[string]string
}

// SecretReader grants read-only credential access scoped strictly to a single (tenantID, connectorType).
// Connectors never receive the global secrets.SecretProvider.
type SecretReader interface {
	Get(ctx context.Context, key string) ([]byte, error)
}

type scopedSecretReader struct {
	provider      secrets.SecretProvider
	tenantID      string
	connectorType string
}

func (s *scopedSecretReader) Get(ctx context.Context, key string) ([]byte, error) {
	return s.provider.Get(ctx, s.tenantID, s.connectorType, key)
}

// CallRequest carries all inputs for invoking a connector operation.
type CallRequest struct {
	TenantID       string
	RunID          string
	StepKey        string
	IdempotencyKey string
	Operation      string
	Params         map[string]any
	Resources      map[string]string
	DryRun         bool
}

// CallResult is the structured outcome of a connector operation.
type CallResult struct {
	Output    map[string]any `json:"output"`
	Simulated bool           `json:"simulated,omitempty"`
	Replayed  bool           `json:"replayed,omitempty"`
}

// Connector is the core interface implemented by every integration (Google Workspace, Notion, Slack, etc.).
type Connector interface {
	Type() string
	Operations() []OperationSpec
	HealthCheck(ctx context.Context) error
	Call(ctx context.Context, req CallRequest) (*CallResult, error)
}

// DeliveryRequest carries a connector-agnostic human interaction prompt (Magic Link / approval card).
type DeliveryRequest struct {
	TenantID        string
	RunID           string
	TaskID          string
	RecipientEmail  string
	RecipientHandle string
	Subject         string
	Body            string
	ActionURL       string
	Metadata        map[string]any
}

// DeliveryReceipt records connector-specific delivery metadata stored in interaction_tasks.delivery_meta.
type DeliveryReceipt struct {
	Provider string         `json:"provider"`
	Meta     map[string]any `json:"meta,omitempty"`
}

// Channel is an optional capability interface implemented by connectors that can deliver
// human-in-the-loop prompts (ask_human / request_approval).
type Channel interface {
	Deliver(ctx context.Context, req DeliveryRequest) (*DeliveryReceipt, error)
}

// Factory instantiates a tenant-configured Connector with a scoped SecretReader.
type Factory func(ctx context.Context, cfg TenantConfig, secretReader SecretReader) (Connector, error)

// ConnectorCatalog summarizes an enabled connector's capabilities and resource aliases
// for the Linter, ADK InstructionProvider, and ConnectRPC API.
type ConnectorCatalog struct {
	Type       string            `json:"type"`
	IsChannel  bool              `json:"is_channel"`
	Operations []OperationSpec   `json:"operations"`
	Resources  map[string]string `json:"resources,omitempty"`
}

// CatalogSnapshot is a read-only view of all enabled connectors for a tenant.
type CatalogSnapshot struct {
	TenantID   string             `json:"tenant_id"`
	Connectors []ConnectorCatalog `json:"connectors"`
}

// ConnectorHealth captures the health check result for a single enabled connector.
type ConnectorHealth struct {
	Type    string `json:"type"`
	Healthy bool   `json:"healthy"`
	Error   string `json:"error,omitempty"`
}

// HealthReport summarizes health checks across all enabled connectors for a tenant.
type HealthReport struct {
	TenantID   string            `json:"tenant_id"`
	Connectors []ConnectorHealth `json:"connectors"`
}

// Healthy returns true when every enabled connector passed HealthCheck.
func (r *HealthReport) Healthy() bool {
	for _, c := range r.Connectors {
		if !c.Healthy {
			return false
		}
	}
	return true
}

// Err returns an actionable combined error if any enabled connector is unhealthy, or nil if all pass.
func (r *HealthReport) Err() error {
	var errs []error
	for _, c := range r.Connectors {
		if !c.Healthy {
			errs = append(errs, fmt.Errorf("connector %q unhealthy: %s", c.Type, c.Error))
		}
	}
	return errors.Join(errs...)
}
