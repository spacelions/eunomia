package googleworkspace_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/spacelions/eunomia/internal/connector"
	"github.com/spacelions/eunomia/internal/connectors/googleworkspace"
	"github.com/spacelions/eunomia/internal/connectorstore"
	"github.com/spacelions/eunomia/internal/secrets"
	"github.com/spacelions/eunomia/test/testpg"
)

type fakeGoogleAPI struct {
	mu           sync.Mutex
	adminStatus  int
	gmailStatus  int
	sendStatus   int
	users        map[string]map[string]any
	createCalls  atomic.Int32
	sentMessages []string
	server       *httptest.Server
}

func newFakeGoogleAPI(t *testing.T) *fakeGoogleAPI {
	t.Helper()
	api := &fakeGoogleAPI{
		adminStatus: http.StatusOK,
		gmailStatus: http.StatusOK,
		sendStatus:  http.StatusOK,
		users: map[string]map[string]any{
			"existing@spacelions.com": {
				"id":           "u_existing",
				"primaryEmail": "existing@spacelions.com",
			},
		},
	}

	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/oauth/token" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "ya29.fake-token",
				"token_type":   "Bearer",
				"expires_in":   3600,
			})

		case r.URL.Path == "/admin/directory/v1/users" && r.Method == http.MethodGet:
			if api.adminStatus != http.StatusOK {
				w.WriteHeader(api.adminStatus)
				_, _ = w.Write([]byte(`{"error":"forbidden"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"users": []any{}})

		case strings.HasPrefix(r.URL.Path, "/admin/directory/v1/users/") && r.Method == http.MethodGet:
			if api.adminStatus != http.StatusOK {
				w.WriteHeader(api.adminStatus)
				_, _ = w.Write([]byte(`{"error":"admin error"}`))
				return
			}
			email := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/admin/directory/v1/users/"))
			api.mu.Lock()
			u, ok := api.users[email]
			api.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"not found"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(u)

		case r.URL.Path == "/admin/directory/v1/users" && r.Method == http.MethodPost:
			api.createCalls.Add(1)
			var payload map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
			email := strings.ToLower(payload["primaryEmail"].(string))

			api.mu.Lock()
			_, exists := api.users[email]
			if !exists {
				payload["id"] = "gws_" + strings.Split(email, "@")[0]
				api.users[email] = payload
			}
			api.mu.Unlock()

			if exists {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"Entity already exists."}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(payload)

		case r.URL.Path == "/gmail/v1/users/me/profile" && r.Method == http.MethodGet:
			if api.gmailStatus != http.StatusOK {
				w.WriteHeader(api.gmailStatus)
				_, _ = w.Write([]byte(`{"error":"gmail scope missing"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"emailAddress": "ops@spacelions.com"})

		case r.URL.Path == "/gmail/v1/users/me/messages/send" && r.Method == http.MethodPost:
			if api.sendStatus != http.StatusOK {
				w.WriteHeader(api.sendStatus)
				_, _ = w.Write([]byte(`{"error":"` + strings.Repeat("x", 300) + `"}`))
				return
			}
			var body struct {
				Raw string `json:"raw"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			decoded, err := base64.RawURLEncoding.DecodeString(body.Raw)
			require.NoError(t, err)
			api.mu.Lock()
			api.sentMessages = append(api.sentMessages, string(decoded))
			msgIdx := len(api.sentMessages)
			api.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":       "msg_" + string(rune('0'+msgIdx)),
				"threadId": "thr_1",
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.server.Close)
	return api
}

func makeTestServiceAccountJSON(t *testing.T, tokenURL string) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	sa := map[string]string{
		"type":         "service_account",
		"project_id":   "eunomia-test",
		"private_key":  string(pemBytes),
		"client_email": "eunomia-bot@eunomia-test.iam.gserviceaccount.com",
		"token_uri":    tokenURL,
	}
	raw, err := json.Marshal(sa)
	require.NoError(t, err)
	return raw
}

func TestGoogleWorkspaceConnector_FullLifecycle(t *testing.T) {
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
	require.NoError(t, reg.Register(googleworkspace.Type, googleworkspace.Factory))

	api := newFakeGoogleAPI(t)
	saJSON := makeTestServiceAccountJSON(t, api.server.URL+"/oauth/token")

	// 1. Factory config & secret validation errors.
	require.NoError(t, connStore.Upsert(ctx, connectorstore.Record{
		TenantID:      "spacelions",
		ConnectorType: googleworkspace.Type,
		Enabled:       true,
		Settings:      map[string]string{},
	}))
	_, _, err = reg.Resolve(ctx, "spacelions", googleworkspace.Type)
	require.Error(t, err)

	require.NoError(t, connStore.Upsert(ctx, connectorstore.Record{
		TenantID:      "spacelions",
		ConnectorType: googleworkspace.Type,
		Enabled:       true,
		Settings: map[string]string{
			"domain":            "spacelions.com",
			"impersonate_email": "not-an-email",
		},
	}))
	_, _, err = reg.Resolve(ctx, "spacelions", googleworkspace.Type)
	require.Error(t, err)

	// Valid settings without secret fails.
	require.NoError(t, connStore.Upsert(ctx, connectorstore.Record{
		TenantID:      "spacelions",
		ConnectorType: googleworkspace.Type,
		Enabled:       true,
		Settings: map[string]string{
			"domain":            "spacelions.com",
			"impersonate_email": "ops@spacelions.com",
			"admin_base_url":    api.server.URL,
			"gmail_base_url":    api.server.URL,
			"token_url":         api.server.URL + "/oauth/token",
		},
	}))
	_, _, err = reg.Resolve(ctx, "spacelions", googleworkspace.Type)
	require.Error(t, err)

	// Invalid service account JSON fails.
	require.NoError(t, secretProvider.Set(ctx, "spacelions", googleworkspace.Type, "service_account_json", []byte(`{invalid`)))
	_, _, err = reg.Resolve(ctx, "spacelions", googleworkspace.Type)
	require.Error(t, err)

	// Seed valid service account JSON.
	require.NoError(t, secretProvider.Set(ctx, "spacelions", googleworkspace.Type, "service_account_json", saJSON))

	// 2. HealthCheck verifies both Admin Directory & Gmail scopes, and catches scoped failures.
	health, err := reg.CheckTenantHealth(ctx, "spacelions")
	require.NoError(t, err)
	assert.True(t, health.Healthy())

	api.adminStatus = http.StatusForbidden
	badHealth, err := reg.CheckTenantHealth(ctx, "spacelions")
	require.NoError(t, err)
	assert.False(t, badHealth.Healthy())
	assert.Contains(t, badHealth.Err().Error(), "admin.directory")
	api.adminStatus = http.StatusOK

	api.gmailStatus = http.StatusUnauthorized
	badHealth, err = reg.CheckTenantHealth(ctx, "spacelions")
	require.NoError(t, err)
	assert.False(t, badHealth.Healthy())
	assert.Contains(t, badHealth.Err().Error(), "gmail")
	api.gmailStatus = http.StatusOK

	// Catalog reports operations and IsChannel: true.
	cat, err := reg.Catalog(ctx, "spacelions")
	require.NoError(t, err)
	require.Len(t, cat.Connectors, 1)
	assert.Equal(t, googleworkspace.Type, cat.Connectors[0].Type)
	assert.True(t, cat.Connectors[0].IsChannel)

	// 3. username_available (RiskRead): available, taken, validation & 500 error.
	res, err := reg.Execute(ctx, googleworkspace.Type, connector.CallRequest{
		TenantID:  "spacelions",
		Operation: "username_available",
		Params:    map[string]any{"username_prefix": "alice"},
	})
	require.NoError(t, err)
	assert.Equal(t, true, res.Output["available"])
	assert.Equal(t, "alice@spacelions.com", res.Output["email"])

	res, err = reg.Execute(ctx, googleworkspace.Type, connector.CallRequest{
		TenantID:  "spacelions",
		Operation: "username_available",
		Params:    map[string]any{"email": "existing@spacelions.com"},
	})
	require.NoError(t, err)
	assert.Equal(t, false, res.Output["available"])

	_, err = reg.Execute(ctx, googleworkspace.Type, connector.CallRequest{
		TenantID:  "spacelions",
		Operation: "username_available",
		Params:    map[string]any{"username_prefix": "alice@bad"},
	})
	require.Error(t, err)

	_, err = reg.Execute(ctx, googleworkspace.Type, connector.CallRequest{
		TenantID:  "spacelions",
		Operation: "username_available",
	})
	require.Error(t, err)

	api.adminStatus = http.StatusInternalServerError
	_, err = reg.Execute(ctx, googleworkspace.Type, connector.CallRequest{
		TenantID:  "spacelions",
		Operation: "username_available",
		Params:    map[string]any{"username_prefix": "alice"},
	})
	require.Error(t, err)
	api.adminStatus = http.StatusOK

	// 4. create_user (RiskWrite): DryRun, happy path, ledger replay, and direct 409 idempotency check.
	dryRes, err := reg.Execute(ctx, googleworkspace.Type, connector.CallRequest{
		TenantID:  "spacelions",
		Operation: "create_user",
		DryRun:    true,
		Params:    map[string]any{"username_prefix": "alice", "given_name": "Alice", "family_name": "Wang"},
	})
	require.NoError(t, err)
	assert.True(t, dryRes.Simulated)

	_, err = reg.Execute(ctx, googleworkspace.Type, connector.CallRequest{
		TenantID:  "spacelions",
		RunID:     "run_01",
		StepKey:   "bad_user",
		Operation: "create_user",
		Params:    map[string]any{"username_prefix": "alice"}, // missing given/family name
	})
	require.Error(t, err)

	createReq := connector.CallRequest{
		TenantID:  "spacelions",
		RunID:     "run_01jcreate",
		StepKey:   "create_gws_user",
		Operation: "create_user",
		Params: map[string]any{
			"username_prefix":               "alice",
			"given_name":                    "Alice",
			"family_name":                   "Wang",
			"org_unit_path":                 "/Engineering",
			"change_password_at_next_login": false,
		},
	}
	created, err := reg.Execute(ctx, googleworkspace.Type, createReq)
	require.NoError(t, err)
	assert.Equal(t, "alice@spacelions.com", created.Output["primary_email"])
	assert.Equal(t, false, created.Output["already_existed"])
	assert.NotEmpty(t, created.Output["temp_password"])
	callsAfterCreate := api.createCalls.Load()

	// Replay same RunID:StepKey through Registry -> returns cached response without API call.
	replayed, err := reg.Execute(ctx, googleworkspace.Type, createReq)
	require.NoError(t, err)
	assert.True(t, replayed.Replayed)
	assert.Equal(t, callsAfterCreate, api.createCalls.Load())

	// Direct 409 retry with same IdempotencyKey succeeds idempotently (`already_existed: true`),
	// while a different IdempotencyKey fails with conflict instead of hijacking!
	conn, _, err := reg.Resolve(ctx, "spacelions", googleworkspace.Type)
	require.NoError(t, err)

	idempotent409, err := conn.Call(ctx, connector.CallRequest{
		TenantID:       "spacelions",
		IdempotencyKey: "run_01jcreate:create_gws_user",
		Operation:      "create_user",
		Params: map[string]any{
			"primary_email": "alice@spacelions.com",
			"given_name":    "Alice",
			"family_name":   "Wang",
		},
	})
	require.NoError(t, err)
	assert.Equal(t, true, idempotent409.Output["already_existed"])
	assert.Nil(t, idempotent409.Output["temp_password"])

	_, err = conn.Call(ctx, connector.CallRequest{
		TenantID:       "spacelions",
		IdempotencyKey: "run_different:create_gws_user",
		Operation:      "create_user",
		Params: map[string]any{
			"primary_email": "alice@spacelions.com",
			"given_name":    "Alice",
			"family_name":   "Wang",
		},
	})
	require.Error(t, err)

	// 409 without idempotency key also rejected cleanly.
	_, err = conn.Call(ctx, connector.CallRequest{
		TenantID:  "spacelions",
		Operation: "create_user",
		Params: map[string]any{
			"primary_email": "existing@spacelions.com",
			"given_name":    "Existing",
			"family_name":   "User",
		},
	})
	require.Error(t, err)

	// 5. send_email (RiskIrreversible) + Channel.Deliver.
	mailRes, err := reg.Execute(ctx, googleworkspace.Type, connector.CallRequest{
		TenantID:  "spacelions",
		RunID:     "run_01jcreate",
		StepKey:   "send_welcome",
		Operation: "send_email",
		Params: map[string]any{
			"to":       "Alice.Personal@Gmail.com",
			"subject":  "Welcome to Space Lions!",
			"body":     "Hello Alice",
			"cc":       []any{"fu@spacelions.com"},
			"reply_to": "hr@spacelions.com",
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "alice.personal@gmail.com", mailRes.Output["to"])
	assert.NotEmpty(t, mailRes.Output["message_id"])

	// Rejects header injection & invalid email / empty subject / empty body.
	_, err = reg.Execute(ctx, googleworkspace.Type, connector.CallRequest{
		TenantID:  "spacelions",
		RunID:     "run_01jcreate",
		StepKey:   "bad_header",
		Operation: "send_email",
		Params: map[string]any{
			"to":      "alice@spacelions.com",
			"subject": "Bad\r\nBcc: attacker@evil.com",
			"body":    "Hello",
		},
	})
	require.Error(t, err)

	_, err = conn.Call(ctx, connector.CallRequest{
		TenantID:  "spacelions",
		Operation: "send_email",
		Params:    map[string]any{"to": "bad-email", "subject": "Hi", "body": "Body"},
	})
	require.Error(t, err)

	_, err = conn.Call(ctx, connector.CallRequest{
		TenantID:  "spacelions",
		Operation: "send_email",
		Params:    map[string]any{"to": "alice@spacelions.com", "subject": "", "body": "Body"},
	})
	require.Error(t, err)

	_, err = conn.Call(ctx, connector.CallRequest{
		TenantID:  "spacelions",
		Operation: "send_email",
		Params:    map[string]any{"to": "alice@spacelions.com", "subject": "Hi", "body": ""},
	})
	require.Error(t, err)

	_, err = conn.Call(ctx, connector.CallRequest{
		TenantID:  "spacelions",
		Operation: "unknown_op",
	})
	require.ErrorIs(t, err, connector.ErrUnknownOperation)

	// Channel delivery appends ActionURL cleanly.
	ch, err := reg.ResolveChannel(ctx, "spacelions", googleworkspace.Type)
	require.NoError(t, err)
	receipt, err := ch.Deliver(ctx, connector.DeliveryRequest{
		TenantID:       "spacelions",
		RunID:          "run_01jcreate",
		TaskID:         "task_01",
		RecipientEmail: "fu@spacelions.com",
		Body:           "Please review and approve the welcome email draft.",
		ActionURL:      "https://eunomia.internal/t/abc123",
	})
	require.NoError(t, err)
	assert.Equal(t, googleworkspace.Type, receipt.Provider)

	api.mu.Lock()
	lastMsg := api.sentMessages[len(api.sentMessages)-1]
	api.mu.Unlock()
	assert.Contains(t, lastMsg, "Action link: https://eunomia.internal/t/abc123")
	assert.Contains(t, lastMsg, "Subject: Action required")

	// Gmail 500 & huge body truncation surface cleanly.
	api.sendStatus = http.StatusInternalServerError
	_, err = ch.Deliver(ctx, connector.DeliveryRequest{
		RecipientEmail: "fu@spacelions.com",
		Subject:        "Test",
		Body:           "Test body",
	})
	require.Error(t, err)
	api.sendStatus = http.StatusOK

	// Additional edge cases: email validation, cc/reply_to variants, create_user errors, canceled ctx.
	_, err = conn.Call(ctx, connector.CallRequest{
		Operation: "username_available",
		Params:    map[string]any{"email": "bad-email"},
	})
	require.Error(t, err)

	_, err = conn.Call(ctx, connector.CallRequest{
		Operation: "create_user",
		Params:    map[string]any{"email": "bad-email"},
	})
	require.Error(t, err)

	// Explicit password and comma-separated / []string cc & invalid cc/reply_to handling.
	_, err = conn.Call(ctx, connector.CallRequest{
		TenantID:  "spacelions",
		Operation: "create_user",
		Params: map[string]any{
			"username_prefix": "bob",
			"given_name":      "Bob",
			"family_name":     "Lee",
			"password":        "CustomPass123!",
		},
	})
	require.NoError(t, err)

	_, err = conn.Call(ctx, connector.CallRequest{
		Operation: "send_email",
		Params: map[string]any{
			"to":      "alice@spacelions.com",
			"subject": "String CC",
			"body":    "Hi",
			"cc":      "bob@spacelions.com, fu@spacelions.com",
		},
	})
	require.NoError(t, err)

	_, err = conn.Call(ctx, connector.CallRequest{
		Operation: "send_email",
		Params: map[string]any{
			"to":      "alice@spacelions.com",
			"subject": "Slice CC",
			"body":    "Hi",
			"cc":      []string{"fu@spacelions.com"},
		},
	})
	require.NoError(t, err)

	for _, badParams := range []map[string]any{
		{"to": "alice@spacelions.com", "subject": "Hi", "body": "Body", "cc": []any{123}},
		{"to": "alice@spacelions.com", "subject": "Hi", "body": "Body", "cc": 123},
		{"to": "alice@spacelions.com", "subject": "Hi", "body": "Body", "cc": "bad-cc"},
		{"to": "alice@spacelions.com", "subject": "Hi", "body": "Body", "reply_to": "bad-reply"},
	} {
		_, err = conn.Call(ctx, connector.CallRequest{Operation: "send_email", Params: badParams})
		require.Error(t, err)
	}

	// Non-409 create_user error and 409 follow-up GET failure.
	api.adminStatus = http.StatusBadGateway
	_, err = conn.Call(ctx, connector.CallRequest{
		TenantID:       "spacelions",
		IdempotencyKey: "run_409getfail:step",
		Operation:      "create_user",
		Params:         map[string]any{"primary_email": "existing@spacelions.com", "given_name": "E", "family_name": "U"},
	})
	require.Error(t, err)
	api.adminStatus = http.StatusOK

	// Canceled context propagates HTTP transport errors cleanly across HealthCheck & operations.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	require.Error(t, conn.HealthCheck(canceledCtx))
	_, err = conn.Call(canceledCtx, connector.CallRequest{
		Operation: "username_available",
		Params:    map[string]any{"username_prefix": "alice"},
	})
	require.Error(t, err)
	_, err = conn.Call(canceledCtx, connector.CallRequest{
		Operation: "create_user",
		Params:    map[string]any{"username_prefix": "newuser", "given_name": "N", "family_name": "U"},
	})
	require.Error(t, err)
	_, err = ch.Deliver(canceledCtx, connector.DeliveryRequest{
		RecipientEmail: "fu@spacelions.com",
		Subject:        "Test",
		Body:           "Test body",
	})
	require.Error(t, err)
}
