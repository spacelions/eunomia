package slack_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/spacelions/eunomia/internal/connector"
	"github.com/spacelions/eunomia/internal/connectors/slack"
	"github.com/spacelions/eunomia/internal/connectorstore"
	"github.com/spacelions/eunomia/internal/secrets"
	"github.com/spacelions/eunomia/test/testpg"
)

func TestSlackConnector_EnabledToggle_Health_AndScaffolds(t *testing.T) {
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
	require.NoError(t, reg.Register(slack.Type, slack.Factory))

	var authMode atomic.Int32 // 0=ok, 1=slack error, 2=http 500, 3=bad json
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth.test" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer xoxb-test-token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_auth"})
			return
		}
		switch authMode.Load() {
		case 1:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "token_revoked"})
		case 2:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`internal error`))
		case 3:
			_, _ = w.Write([]byte(`{invalid`))
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "team_id": "T123"})
		}
	}))
	t.Cleanup(server.Close)

	// 1. Disabled by default: excluded from Catalog & CheckTenantHealth, Resolve returns ErrConnectorDisabled.
	require.NoError(t, connStore.Upsert(ctx, connectorstore.Record{
		TenantID:      "spacelions",
		ConnectorType: slack.Type,
		Enabled:       false,
		Settings:      map[string]string{"base_url": server.URL},
		Resources:     map[string]string{"it_ops_channel": "C01234"},
	}))

	cat, err := reg.Catalog(ctx, "spacelions")
	require.NoError(t, err)
	assert.Empty(t, cat.Connectors, "disabled slack connector must be excluded from Catalog")

	health, err := reg.CheckTenantHealth(ctx, "spacelions")
	require.NoError(t, err)
	assert.Empty(t, health.Connectors, "disabled slack connector must be excluded from HealthCheck")
	assert.True(t, health.Healthy())

	_, _, err = reg.Resolve(ctx, "spacelions", slack.Type)
	require.ErrorIs(t, err, connector.ErrConnectorDisabled)

	// 2. Enable without secrets -> Factory rejects missing bot_token / empty bot_token / missing app_token.
	require.NoError(t, connStore.SetEnabled(ctx, "spacelions", slack.Type, true))
	_, _, err = reg.Resolve(ctx, "spacelions", slack.Type)
	require.Error(t, err)

	require.NoError(t, secretProvider.Set(ctx, "spacelions", slack.Type, "bot_token", []byte("   ")))
	_, _, err = reg.Resolve(ctx, "spacelions", slack.Type)
	require.Error(t, err)

	require.NoError(t, secretProvider.Set(ctx, "spacelions", slack.Type, "bot_token", []byte("xoxb-test-token")))
	_, _, err = reg.Resolve(ctx, "spacelions", slack.Type)
	require.Error(t, err, "socket_mode defaults to true so missing app_token must fail")

	require.NoError(t, secretProvider.Set(ctx, "spacelions", slack.Type, "app_token", []byte("   ")))
	_, _, err = reg.Resolve(ctx, "spacelions", slack.Type)
	require.Error(t, err)

	// With socket_mode=false, app_token is not required.
	require.NoError(t, connStore.Upsert(ctx, connectorstore.Record{
		TenantID:      "spacelions",
		ConnectorType: slack.Type,
		Enabled:       true,
		Settings:      map[string]string{"base_url": server.URL, "socket_mode": "false"},
		Resources:     map[string]string{"it_ops_channel": "C01234"},
	}))
	noSocketConn, _, err := reg.Resolve(ctx, "spacelions", slack.Type)
	require.NoError(t, err)
	assert.False(t, noSocketConn.(*slack.Connector).SocketModeEnabled())

	// Enable full Socket Mode with valid app_token.
	require.NoError(t, secretProvider.Set(ctx, "spacelions", slack.Type, "app_token", []byte("xapp-test-token")))
	require.NoError(t, connStore.Upsert(ctx, connectorstore.Record{
		TenantID:      "spacelions",
		ConnectorType: slack.Type,
		Enabled:       true,
		Settings:      map[string]string{"base_url": server.URL},
		Resources:     map[string]string{"it_ops_channel": "C01234"},
	}))
	conn, _, err := reg.Resolve(ctx, "spacelions", slack.Type)
	require.NoError(t, err)
	assert.True(t, conn.(*slack.Connector).SocketModeEnabled())

	// 3. Catalog exposes IsChannel: true, operations, and channel aliases.
	cat, err = reg.Catalog(ctx, "spacelions")
	require.NoError(t, err)
	require.Len(t, cat.Connectors, 1)
	assert.Equal(t, slack.Type, cat.Connectors[0].Type)
	assert.True(t, cat.Connectors[0].IsChannel)
	assert.Equal(t, "C01234", cat.Connectors[0].Resources["it_ops_channel"])
	assert.Len(t, cat.Connectors[0].Operations, 2)

	// 4. HealthCheck pass + error diagnostics.
	health, err = reg.CheckTenantHealth(ctx, "spacelions")
	require.NoError(t, err)
	assert.True(t, health.Healthy())

	for _, mode := range []int32{1, 2, 3} {
		authMode.Store(mode)
		bad, hErr := reg.CheckTenantHealth(ctx, "spacelions")
		require.NoError(t, hErr)
		assert.False(t, bad.Healthy())
	}
	authMode.Store(0)

	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	require.Error(t, conn.HealthCheck(canceledCtx))

	// 5. Scaffolded operations: DryRun simulates cleanly; live calls validate params and return ErrNotImplemented.
	dryRes, err := reg.Execute(ctx, slack.Type, connector.CallRequest{
		TenantID:  "spacelions",
		Operation: "post_message",
		DryRun:    true,
		Params:    map[string]any{"channel": "it_ops_channel", "text": "Provision laptop"},
	})
	require.NoError(t, err)
	assert.True(t, dryRes.Simulated)

	for _, badReq := range []connector.CallRequest{
		{Operation: "post_message", Params: map[string]any{"text": "hi"}},
		{Operation: "post_message", Params: map[string]any{"channel": "it_ops_channel"}},
		{Operation: "invite_to_channel", Params: map[string]any{"user_id": "U123"}},
		{Operation: "invite_to_channel", Params: map[string]any{"channel": "C01234"}},
	} {
		_, err = conn.Call(ctx, badReq)
		require.Error(t, err)
		assert.NotErrorIs(t, err, slack.ErrNotImplemented)
	}

	_, err = reg.Execute(ctx, slack.Type, connector.CallRequest{
		TenantID:  "spacelions",
		RunID:     "run_01jslack",
		StepKey:   "notify_it",
		Operation: "post_message",
		Params:    map[string]any{"channel": "it_ops_channel", "text": "Please prepare MacBook"},
	})
	require.ErrorIs(t, err, slack.ErrNotImplemented)

	_, err = conn.Call(ctx, connector.CallRequest{
		Operation: "invite_to_channel",
		Params:    map[string]any{"channel": "C999", "user_id": "U123"},
	})
	require.ErrorIs(t, err, slack.ErrNotImplemented)

	_, err = conn.Call(ctx, connector.CallRequest{Operation: "unknown_op"})
	require.ErrorIs(t, err, connector.ErrUnknownOperation)

	// 6. Channel.Deliver validates RecipientHandle and returns ErrNotImplemented.
	ch, err := reg.ResolveChannel(ctx, "spacelions", slack.Type)
	require.NoError(t, err)
	_, err = ch.Deliver(ctx, connector.DeliveryRequest{})
	require.Error(t, err)
	_, err = ch.Deliver(ctx, connector.DeliveryRequest{RecipientHandle: "U_FU", Body: "Approve draft"})
	require.ErrorIs(t, err, slack.ErrNotImplemented)
}
