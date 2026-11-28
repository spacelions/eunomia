package connector_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/spacelions/eunomia/internal/connector"
	"github.com/spacelions/eunomia/internal/connectorstore"
	"github.com/spacelions/eunomia/internal/secrets"
	"github.com/spacelions/eunomia/test/testpg"
)

type stubConnector struct {
	typ       string
	ops       []connector.OperationSpec
	healthErr error
	callFn    func(ctx context.Context, req connector.CallRequest) (*connector.CallResult, error)
}

func (s *stubConnector) Type() string                          { return s.typ }
func (s *stubConnector) Operations() []connector.OperationSpec { return s.ops }
func (s *stubConnector) HealthCheck(context.Context) error     { return s.healthErr }
func (s *stubConnector) Call(ctx context.Context, req connector.CallRequest) (*connector.CallResult, error) {
	return s.callFn(ctx, req)
}

type stubChannelConnector struct {
	stubConnector
	deliverFn func(ctx context.Context, req connector.DeliveryRequest) (*connector.DeliveryReceipt, error)
}

func (s *stubChannelConnector) Deliver(ctx context.Context, req connector.DeliveryRequest) (*connector.DeliveryReceipt, error) {
	return s.deliverFn(ctx, req)
}

func TestTypesAndIdempotencyKeyHelpers(t *testing.T) {
	assert.True(t, connector.RiskRead.IsValid())
	assert.True(t, connector.RiskWrite.IsValid())
	assert.True(t, connector.RiskIrreversible.IsValid())
	assert.False(t, connector.RiskTier("bogus").IsValid())

	assert.False(t, connector.RiskRead.RequiresIdempotency())
	assert.True(t, connector.RiskWrite.RequiresIdempotency())
	assert.True(t, connector.RiskIrreversible.RequiresIdempotency())

	key, err := connector.FormatIdempotencyKey("run_01j", "create_user")
	require.NoError(t, err)
	assert.Equal(t, "run_01j:create_user", key)

	_, err = connector.FormatIdempotencyKey("", "step")
	require.ErrorIs(t, err, connector.ErrIdempotencyKeyRequired)
	_, err = connector.FormatIdempotencyKey("run_01j", " ")
	require.ErrorIs(t, err, connector.ErrIdempotencyKeyRequired)
}

func TestRegistry_SecretsIsolation_Catalog_Health_AndIdempotencyLedger(t *testing.T) {
	_, database := testpg.NewMigrated(t)
	ctx := context.Background()

	k1, err := secrets.GenerateLocalKey("v1")
	require.NoError(t, err)
	wrapper, err := secrets.ParseLocalKeyWrapper(k1)
	require.NoError(t, err)

	secretProvider := secrets.NewDBSecretProvider(database, wrapper)
	connStore := connectorstore.New(database)
	ledger := connector.NewLedger(database)
	reg := connector.NewRegistry(connStore, secretProvider, ledger)

	// Register validation.
	require.Error(t, reg.Register("", func(context.Context, connector.TenantConfig, connector.SecretReader) (connector.Connector, error) {
		return nil, nil
	}))
	require.Error(t, reg.Register("notion", nil))

	// Seed per-tenant secrets & connector configs.
	require.NoError(t, secretProvider.Set(ctx, "spacelions", "notion", "api_token", []byte("ntn_secret_123")))
	require.NoError(t, secretProvider.Set(ctx, "spacelions", "gmail", "oauth_token", []byte("gmail_secret_456")))
	require.NoError(t, secretProvider.Set(ctx, "other_tenant", "notion", "api_token", []byte("other_secret")))

	require.NoError(t, connStore.Upsert(ctx, connectorstore.Record{
		TenantID:      "spacelions",
		ConnectorType: "notion",
		Enabled:       true,
		Settings:      map[string]string{"workspace": "spacelions"},
		Resources:     map[string]string{"onboarding_tracker": "db_abc123"},
	}))
	require.NoError(t, connStore.Upsert(ctx, connectorstore.Record{
		TenantID:      "spacelions",
		ConnectorType: "gmail",
		Enabled:       true,
		Settings:      map[string]string{"sender": "ops@spacelions.com"},
	}))
	require.NoError(t, connStore.Upsert(ctx, connectorstore.Record{
		TenantID:      "spacelions",
		ConnectorType: "disabled_tool",
		Enabled:       false,
	}))

	var notionCalls atomic.Int32
	var shouldFailOnce atomic.Bool
	shouldFailOnce.Store(true)

	require.NoError(t, reg.Register("notion", func(ctx context.Context, cfg connector.TenantConfig, sr connector.SecretReader) (connector.Connector, error) {
		tok, err := sr.Get(ctx, "api_token")
		if err != nil {
			return nil, err
		}
		// Prove scoped SecretReader cannot read another connector's secret key.
		if _, crossErr := sr.Get(ctx, "oauth_token"); !errors.Is(crossErr, secrets.ErrSecretNotFound) {
			return nil, errors.New("expected scoped secret reader to isolate connectors")
		}
		if string(tok) != "ntn_secret_123" {
			return nil, errors.New("unexpected notion token")
		}
		return &stubConnector{
			typ: "notion",
			ops: []connector.OperationSpec{
				{Name: "query_db", Description: "Query Notion DB", RiskTier: connector.RiskRead},
				{Name: "upsert_db_row", Description: "Upsert Notion row", RiskTier: connector.RiskWrite},
				{Name: "bad_tier_op", Description: "Invalid tier", RiskTier: connector.RiskTier("bad")},
			},
			callFn: func(_ context.Context, req connector.CallRequest) (*connector.CallResult, error) {
				n := notionCalls.Add(1)
				if req.Operation == "upsert_db_row" && shouldFailOnce.Swap(false) {
					return nil, errors.New("transient 503")
				}
				return &connector.CallResult{
					Output: map[string]any{
						"call_num":   float64(n),
						"tracker_id": req.Resources["onboarding_tracker"],
						"op":         req.Operation,
					},
				}, nil
			},
		}, nil
	}))

	// Duplicate registration rejected.
	require.Error(t, reg.Register("notion", func(context.Context, connector.TenantConfig, connector.SecretReader) (connector.Connector, error) {
		return nil, nil
	}))

	var gmailHealthy atomic.Bool
	gmailHealthy.Store(true)
	require.NoError(t, reg.Register("gmail", func(ctx context.Context, _ connector.TenantConfig, sr connector.SecretReader) (connector.Connector, error) {
		if _, err := sr.Get(ctx, "oauth_token"); err != nil {
			return nil, err
		}
		var hErr error
		if !gmailHealthy.Load() {
			hErr = errors.New("oauth token expired")
		}
		return &stubChannelConnector{
			stubConnector: stubConnector{
				typ:       "gmail",
				healthErr: hErr,
				ops: []connector.OperationSpec{
					{Name: "send_email", Description: "Send email", RiskTier: connector.RiskIrreversible},
				},
				callFn: func(_ context.Context, _ connector.CallRequest) (*connector.CallResult, error) {
					return nil, nil // tests nil result normalization
				},
			},
			deliverFn: func(_ context.Context, req connector.DeliveryRequest) (*connector.DeliveryReceipt, error) {
				return &connector.DeliveryReceipt{
					Provider: "gmail",
					Meta:     map[string]any{"to": req.RecipientEmail},
				}, nil
			},
		}, nil
	}))
	require.NoError(t, reg.Register("disabled_tool", func(context.Context, connector.TenantConfig, connector.SecretReader) (connector.Connector, error) {
		return &stubConnector{typ: "disabled_tool"}, nil
	}))

	assert.Empty(t, cmp.Diff([]string{"disabled_tool", "gmail", "notion"}, reg.RegisteredTypes()))

	// Resolve errors: unknown connector, disabled connector, unconfigured tenant.
	_, _, err = reg.Resolve(ctx, "spacelions", "linear")
	require.ErrorIs(t, err, connector.ErrUnknownConnector)
	_, _, err = reg.Resolve(ctx, "spacelions", "disabled_tool")
	require.ErrorIs(t, err, connector.ErrConnectorDisabled)
	_, _, err = reg.Resolve(ctx, "missing_tenant", "notion")
	require.ErrorIs(t, err, connector.ErrConnectorDisabled)

	// ResolveChannel: gmail supports Channel, notion does not.
	ch, err := reg.ResolveChannel(ctx, "spacelions", "gmail")
	require.NoError(t, err)
	receipt, err := ch.Deliver(ctx, connector.DeliveryRequest{RecipientEmail: "alice@spacelions.com"})
	require.NoError(t, err)
	assert.Equal(t, "gmail", receipt.Provider)

	_, err = reg.ResolveChannel(ctx, "spacelions", "notion")
	require.ErrorIs(t, err, connector.ErrNotAChannel)
	_, err = reg.ResolveChannel(ctx, "spacelions", "linear")
	require.ErrorIs(t, err, connector.ErrUnknownConnector)

	// 1. CheckTenantHealth: all healthy initially.
	report, err := reg.CheckTenantHealth(ctx, "spacelions")
	require.NoError(t, err)
	assert.True(t, report.Healthy())
	assert.NoError(t, report.Err())
	require.Len(t, report.Connectors, 2)

	// Catalog snapshot lists enabled connectors, channel capability, operations, and aliases.
	catalog, err := reg.Catalog(ctx, "spacelions")
	require.NoError(t, err)
	require.Len(t, catalog.Connectors, 2)
	assert.Equal(t, "gmail", catalog.Connectors[0].Type)
	assert.True(t, catalog.Connectors[0].IsChannel)
	assert.Equal(t, "notion", catalog.Connectors[1].Type)
	assert.False(t, catalog.Connectors[1].IsChannel)
	assert.Equal(t, "db_abc123", catalog.Connectors[1].Resources["onboarding_tracker"])

	// Unhealthy connector + factory-init-error connector + unregistered enabled connector reported cleanly in HealthReport.
	gmailHealthy.Store(false)
	require.NoError(t, reg.Register("broken_init", func(context.Context, connector.TenantConfig, connector.SecretReader) (connector.Connector, error) {
		return nil, errors.New("missing api_key secret")
	}))
	require.NoError(t, connStore.Upsert(ctx, connectorstore.Record{
		TenantID:      "spacelions",
		ConnectorType: "broken_init",
		Enabled:       true,
	}))
	require.NoError(t, connStore.Upsert(ctx, connectorstore.Record{
		TenantID:      "spacelions",
		ConnectorType: "unregistered_enabled",
		Enabled:       true,
	}))
	badReport, err := reg.CheckTenantHealth(ctx, "spacelions")
	require.NoError(t, err)
	assert.False(t, badReport.Healthy())
	require.Error(t, badReport.Err())
	assert.Contains(t, badReport.Err().Error(), "oauth token expired")
	assert.Contains(t, badReport.Err().Error(), "missing api_key secret")
	assert.Contains(t, badReport.Err().Error(), "unregistered_enabled")

	// Catalog fails when an enabled connector fails init or has no registered factory.
	_, err = reg.Catalog(ctx, "spacelions")
	require.Error(t, err)
	require.NoError(t, connStore.SetEnabled(ctx, "spacelions", "broken_init", false))
	_, err = reg.Catalog(ctx, "spacelions")
	require.ErrorIs(t, err, connector.ErrUnknownConnector)

	// Restore clean enabled set.
	gmailHealthy.Store(true)
	require.NoError(t, connStore.SetEnabled(ctx, "spacelions", "unregistered_enabled", false))

	// 2. Execute: RiskRead bypasses ledger and passes resource aliases.
	readRes, err := reg.Execute(ctx, "notion", connector.CallRequest{
		TenantID:  "spacelions",
		Operation: "query_db",
	})
	require.NoError(t, err)
	assert.False(t, readRes.Simulated)
	assert.False(t, readRes.Replayed)
	assert.Equal(t, "db_abc123", readRes.Output["tracker_id"])

	// Unknown op & invalid tier rejected.
	_, err = reg.Execute(ctx, "notion", connector.CallRequest{TenantID: "spacelions", Operation: "nonexistent"})
	require.ErrorIs(t, err, connector.ErrUnknownOperation)
	_, err = reg.Execute(ctx, "notion", connector.CallRequest{TenantID: "spacelions", Operation: "bad_tier_op"})
	require.Error(t, err)

	// 3. Execute: DryRun simulates write/irreversible without invoking connector or writing ledger.
	beforeCalls := notionCalls.Load()
	dryRes, err := reg.Execute(ctx, "notion", connector.CallRequest{
		TenantID:  "spacelions",
		Operation: "upsert_db_row",
		DryRun:    true,
	})
	require.NoError(t, err)
	assert.True(t, dryRes.Simulated)
	assert.Equal(t, beforeCalls, notionCalls.Load(), "dry_run must not invoke connector")

	// 4. Execute: missing idempotency key on live write rejected.
	_, err = reg.Execute(ctx, "notion", connector.CallRequest{
		TenantID:  "spacelions",
		Operation: "upsert_db_row",
	})
	require.ErrorIs(t, err, connector.ErrIdempotencyKeyRequired)

	// 5. Execute: transient failure marks `failed`, retry reclaims & succeeds, third call replays cached output!
	req := connector.CallRequest{
		TenantID:  "spacelions",
		RunID:     "run_01jtest",
		StepKey:   "update_tracker",
		Operation: "upsert_db_row",
	}
	_, err = reg.Execute(ctx, "notion", req)
	require.Error(t, err)

	liveRes, err := reg.Execute(ctx, "notion", req)
	require.NoError(t, err)
	assert.False(t, liveRes.Replayed)
	callsAfterSuccess := notionCalls.Load()

	replayRes, err := reg.Execute(ctx, "notion", req)
	require.NoError(t, err)
	assert.True(t, replayRes.Replayed)
	assert.Equal(t, liveRes.Output, replayRes.Output)
	assert.Equal(t, callsAfterSuccess, notionCalls.Load(), "replayed execution must NOT re-invoke connector")

	// Irreversible nil-output normalization + ledger audit ListByRun.
	mailRes, err := reg.Execute(ctx, "gmail", connector.CallRequest{
		TenantID:  "spacelions",
		RunID:     "run_01jtest",
		StepKey:   "welcome_email",
		Operation: "send_email",
	})
	require.NoError(t, err)
	assert.NotNil(t, mailRes.Output)

	history, err := ledger.ListByRun(ctx, "spacelions", "run_01jtest")
	require.NoError(t, err)
	require.Len(t, history, 2)
	assert.Equal(t, connector.ExecutionSucceeded, history[0].Status)
	assert.Equal(t, connector.ExecutionSucceeded, history[1].Status)

	// Reusing an existing idempotency key for a different connector/operation MUST fail with ErrIdempotencyKeyMismatch.
	_, err = reg.Execute(ctx, "gmail", connector.CallRequest{
		TenantID:       "spacelions",
		IdempotencyKey: "run_01jtest:update_tracker",
		RunID:          "run_01jtest",
		Operation:      "send_email",
	})
	require.ErrorIs(t, err, connector.ErrIdempotencyKeyMismatch)

	// Mid-call ctx cancellation MUST finalize the ledger cleanly instead of stranding `in_flight`.
	var midCancelMu sync.Mutex
	var midCancelFn context.CancelFunc
	var failOnCancel atomic.Bool
	failOnCancel.Store(true)

	require.NoError(t, reg.Register("cancellable", func(context.Context, connector.TenantConfig, connector.SecretReader) (connector.Connector, error) {
		return &stubConnector{
			typ: "cancellable",
			ops: []connector.OperationSpec{
				{Name: "mutate", RiskTier: connector.RiskWrite},
			},
			callFn: func(callCtx context.Context, _ connector.CallRequest) (*connector.CallResult, error) {
				midCancelMu.Lock()
				fn := midCancelFn
				midCancelMu.Unlock()
				if fn != nil {
					fn()
				}
				if failOnCancel.Load() {
					return nil, callCtx.Err()
				}
				return &connector.CallResult{Output: map[string]any{"done": true}}, nil
			},
		}, nil
	}))
	require.NoError(t, connStore.Upsert(ctx, connectorstore.Record{
		TenantID:      "spacelions",
		ConnectorType: "cancellable",
		Enabled:       true,
	}))

	// Case A: call cancels ctx and returns error -> ledger marks `failed`, allowing subsequent reclaim & retry.
	callCtx1, cancel1 := context.WithCancel(ctx)
	midCancelMu.Lock()
	midCancelFn = cancel1
	midCancelMu.Unlock()
	_, err = reg.Execute(callCtx1, "cancellable", connector.CallRequest{
		TenantID:  "spacelions",
		RunID:     "run_01jcancel",
		StepKey:   "step1",
		Operation: "mutate",
	})
	require.ErrorIs(t, err, context.Canceled)

	rows, err := ledger.ListByRun(ctx, "spacelions", "run_01jcancel")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, connector.ExecutionFailed, rows[0].Status)

	// Retry reclaims cleanly; even if ctx cancels just before return, MarkSucceeded still commits cleanly.
	failOnCancel.Store(false)
	callCtx2, cancel2 := context.WithCancel(ctx)
	midCancelMu.Lock()
	midCancelFn = cancel2
	midCancelMu.Unlock()
	res2, err := reg.Execute(callCtx2, "cancellable", connector.CallRequest{
		TenantID:  "spacelions",
		RunID:     "run_01jcancel",
		StepKey:   "step1",
		Operation: "mutate",
	})
	require.NoError(t, err)
	assert.Equal(t, true, res2.Output["done"])

	// Canceled context propagates DB errors cleanly through Resolve, CheckTenantHealth, and Catalog.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, _, err = reg.Resolve(canceledCtx, "spacelions", "notion")
	require.Error(t, err)
	_, err = reg.CheckTenantHealth(canceledCtx, "spacelions")
	require.Error(t, err)
	_, err = reg.Catalog(canceledCtx, "spacelions")
	require.Error(t, err)
	_, err = reg.Execute(canceledCtx, "notion", req)
	require.Error(t, err)
}

func TestLedger_ConcurrentInFlightRejection_AndValidation(t *testing.T) {
	_, database := testpg.NewMigrated(t)
	ctx := context.Background()
	ledger := connector.NewLedger(database)

	// Validation errors.
	_, _, err := ledger.Claim(ctx, connector.ClaimParams{IdempotencyKey: "k", RiskTier: connector.RiskWrite})
	require.Error(t, err)
	_, _, err = ledger.Claim(ctx, connector.ClaimParams{TenantID: "spacelions", RiskTier: connector.RiskWrite})
	require.ErrorIs(t, err, connector.ErrIdempotencyKeyRequired)
	_, _, err = ledger.Claim(ctx, connector.ClaimParams{TenantID: "spacelions", IdempotencyKey: "k", RiskTier: connector.RiskRead})
	require.Error(t, err)

	// Missing key on MarkSucceeded / MarkFailed.
	require.Error(t, ledger.MarkSucceeded(ctx, "spacelions", "missing", nil))
	require.Error(t, ledger.MarkFailed(ctx, "spacelions", "missing", "boom"))

	// Two goroutines race to Claim the same fresh key -> strictly ONE claims, the other gets ErrExecutionInFlight.
	var wg sync.WaitGroup
	claimedFlags := make([]bool, 2)
	errs := make([]error, 2)
	wg.Add(2)
	for i := range 2 {
		go func(idx int) {
			defer wg.Done()
			_, claimedFlags[idx], errs[idx] = ledger.Claim(ctx, connector.ClaimParams{
				TenantID:       "spacelions",
				IdempotencyKey: "run_01jrace:send_email",
				RunID:          "run_01jrace",
				ConnectorType:  "gmail",
				Operation:      "send_email",
				RiskTier:       connector.RiskIrreversible,
			})
		}(i)
	}
	wg.Wait()

	winners := 0
	inFlightRejects := 0
	for i := range 2 {
		if errs[i] == nil && claimedFlags[i] {
			winners++
		} else {
			require.ErrorIs(t, errs[i], connector.ErrExecutionInFlight)
			inFlightRejects++
		}
	}
	assert.Equal(t, 1, winners)
	assert.Equal(t, 1, inFlightRejects)

	// Canceled context propagates DB errors cleanly.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, _, err = ledger.Claim(canceledCtx, connector.ClaimParams{
		TenantID:       "spacelions",
		IdempotencyKey: "run_01j:k",
		ConnectorType:  "gmail",
		Operation:      "send_email",
		RiskTier:       connector.RiskWrite,
	})
	require.Error(t, err)
	require.Error(t, ledger.MarkSucceeded(canceledCtx, "spacelions", "run_01jrace:send_email", nil))
	require.Error(t, ledger.MarkFailed(canceledCtx, "spacelions", "run_01jrace:send_email", "err"))
	_, err = ledger.ListByRun(canceledCtx, "spacelions", "run_01jrace")
	require.Error(t, err)
}
