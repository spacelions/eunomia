package notion_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/spacelions/eunomia/internal/connector"
	"github.com/spacelions/eunomia/internal/connectors/notion"
	"github.com/spacelions/eunomia/internal/connectorstore"
	"github.com/spacelions/eunomia/internal/secrets"
	"github.com/spacelions/eunomia/test/testpg"
)

type fakeNotionPage struct {
	ID         string         `json:"id"`
	URL        string         `json:"url"`
	DatabaseID string         `json:"-"`
	Properties map[string]any `json:"properties"`
}

type fakeNotionAPI struct {
	mu           sync.Mutex
	expectedTok  string
	authFail     bool
	databases    map[string]bool
	pages        map[string]*fakeNotionPage
	nextSeq      int
	queryStatus  int
	createStatus int
	patchStatus  int
	server       *httptest.Server
}

func newFakeNotionAPI(t *testing.T, token string) *fakeNotionAPI {
	t.Helper()
	api := &fakeNotionAPI{
		expectedTok: token,
		databases: map[string]bool{
			"db_tracker_123": true,
		},
		pages:        make(map[string]*fakeNotionPage),
		queryStatus:  http.StatusOK,
		createStatus: http.StatusOK,
		patchStatus:  http.StatusOK,
	}

	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if api.authFail || r.Header.Get("Authorization") != "Bearer "+api.expectedTok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"object":"error","code":"unauthorized"}`))
			return
		}
		if r.Header.Get("Notion-Version") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"missing Notion-Version"}`))
			return
		}

		switch {
		case r.URL.Path == "/v1/users/me" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "user", "id": "bot_1"})

		case strings.HasPrefix(r.URL.Path, "/v1/databases/") && strings.HasSuffix(r.URL.Path, "/query") && r.Method == http.MethodPost:
			if api.queryStatus != http.StatusOK {
				w.WriteHeader(api.queryStatus)
				_, _ = w.Write([]byte(`{"error":"query failed"}`))
				return
			}
			dbID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/databases/"), "/query")
			var req struct {
				Filter map[string]any `json:"filter"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)

			var runIDFilter string
			if req.Filter != nil {
				if rt, ok := req.Filter["rich_text"].(map[string]any); ok {
					runIDFilter, _ = rt["equals"].(string)
				}
			}

			api.mu.Lock()
			var results []*fakeNotionPage
			for _, p := range api.pages {
				if p.DatabaseID != dbID {
					continue
				}
				if runIDFilter != "" && extractRunID(p.Properties["Run ID"]) != runIDFilter {
					continue
				}
				results = append(results, p)
			}
			api.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"results":  results,
				"has_more": false,
			})

		case strings.HasPrefix(r.URL.Path, "/v1/databases/") && r.Method == http.MethodGet:
			dbID := strings.TrimPrefix(r.URL.Path, "/v1/databases/")
			api.mu.Lock()
			ok := api.databases[dbID]
			api.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"database not found"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "database", "id": dbID})

		case r.URL.Path == "/v1/pages" && r.Method == http.MethodPost:
			if api.createStatus != http.StatusOK {
				w.WriteHeader(api.createStatus)
				_, _ = w.Write([]byte(`{"error":"create failed"}`))
				return
			}
			var req struct {
				Parent struct {
					DatabaseID string `json:"database_id"`
				} `json:"parent"`
				Properties map[string]any `json:"properties"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))

			api.mu.Lock()
			api.nextSeq++
			id := "page_" + string(rune('0'+api.nextSeq))
			page := &fakeNotionPage{
				ID:         id,
				URL:        "https://www.notion.so/" + id,
				DatabaseID: req.Parent.DatabaseID,
				Properties: req.Properties,
			}
			api.pages[id] = page
			api.mu.Unlock()
			_ = json.NewEncoder(w).Encode(page)

		case strings.HasPrefix(r.URL.Path, "/v1/pages/") && r.Method == http.MethodPatch:
			if api.patchStatus != http.StatusOK {
				w.WriteHeader(api.patchStatus)
				_, _ = w.Write([]byte(`{"error":"patch failed"}`))
				return
			}
			pageID := strings.TrimPrefix(r.URL.Path, "/v1/pages/")
			var req struct {
				Properties map[string]any `json:"properties"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))

			api.mu.Lock()
			page, exists := api.pages[pageID]
			if exists {
				for k, v := range req.Properties {
					page.Properties[k] = v
				}
			}
			api.mu.Unlock()
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(page)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.server.Close)
	return api
}

func extractRunID(prop any) string {
	m, ok := prop.(map[string]any)
	if !ok {
		return ""
	}
	arr, ok := m["rich_text"].([]any)
	if !ok || len(arr) == 0 {
		return ""
	}
	item, ok := arr[0].(map[string]any)
	if !ok {
		return ""
	}
	txt, _ := item["text"].(map[string]any)
	content, _ := txt["content"].(string)
	return content
}

func TestNotionConnector_FullLifecycle(t *testing.T) {
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
	require.NoError(t, reg.Register(notion.Type, notion.Factory))

	api := newFakeNotionAPI(t, "ntn_live_token")

	// 1. Missing/empty api_token secret fails Resolve.
	require.NoError(t, connStore.Upsert(ctx, connectorstore.Record{
		TenantID:      "spacelions",
		ConnectorType: notion.Type,
		Enabled:       true,
		Settings: map[string]string{
			"base_url":        api.server.URL,
			"notion_version":  "2022-06-28",
			"run_id_property": "Run ID",
		},
		Resources: map[string]string{
			"onboarding_tracker": "db_tracker_123",
		},
	}))
	_, _, err = reg.Resolve(ctx, "spacelions", notion.Type)
	require.Error(t, err)

	require.NoError(t, secretProvider.Set(ctx, "spacelions", notion.Type, "api_token", []byte("   ")))
	_, _, err = reg.Resolve(ctx, "spacelions", notion.Type)
	require.Error(t, err)

	require.NoError(t, secretProvider.Set(ctx, "spacelions", notion.Type, "api_token", []byte("ntn_live_token")))

	// 2. HealthCheck verifies token AND every configured DB alias.
	health, err := reg.CheckTenantHealth(ctx, "spacelions")
	require.NoError(t, err)
	assert.True(t, health.Healthy())

	api.authFail = true
	badHealth, err := reg.CheckTenantHealth(ctx, "spacelions")
	require.NoError(t, err)
	assert.False(t, badHealth.Healthy())
	api.authFail = false

	// Missing/inaccessible DB alias detected in HealthCheck.
	require.NoError(t, connStore.Upsert(ctx, connectorstore.Record{
		TenantID:      "spacelions",
		ConnectorType: notion.Type,
		Enabled:       true,
		Settings:      map[string]string{"base_url": api.server.URL},
		Resources: map[string]string{
			"onboarding_tracker": "db_tracker_123",
			"broken_alias":       "db_missing_999",
		},
	}))
	badHealth, err = reg.CheckTenantHealth(ctx, "spacelions")
	require.NoError(t, err)
	assert.False(t, badHealth.Healthy())
	assert.Contains(t, badHealth.Err().Error(), "broken_alias")

	require.NoError(t, connStore.Upsert(ctx, connectorstore.Record{
		TenantID:      "spacelions",
		ConnectorType: notion.Type,
		Enabled:       true,
		Settings:      map[string]string{"base_url": api.server.URL},
		Resources:     map[string]string{"onboarding_tracker": "db_tracker_123"},
	}))

	// Catalog lists operations & resources (IsChannel: false).
	cat, err := reg.Catalog(ctx, "spacelions")
	require.NoError(t, err)
	require.Len(t, cat.Connectors, 1)
	assert.False(t, cat.Connectors[0].IsChannel)
	assert.Equal(t, "db_tracker_123", cat.Connectors[0].Resources["onboarding_tracker"])

	// 3. upsert_db_row (RiskWrite): DryRun, create -> update same run_id without duplicates!
	dryRes, err := reg.Execute(ctx, notion.Type, connector.CallRequest{
		TenantID:  "spacelions",
		Operation: "upsert_db_row",
		DryRun:    true,
		Params:    map[string]any{"database": "onboarding_tracker"},
	})
	require.NoError(t, err)
	assert.True(t, dryRes.Simulated)

	created, err := reg.Execute(ctx, notion.Type, connector.CallRequest{
		TenantID:  "spacelions",
		RunID:     "run_01jonboard",
		StepKey:   "tracker_initial",
		Operation: "upsert_db_row",
		Params: map[string]any{
			"database": "onboarding_tracker",
			"title":    "Alice Wang Onboarding",
			"properties": map[string]any{
				"Status":         map[string]any{"status": map[string]any{"name": "In Progress"}},
				"Department":     "Engineering",
				"Laptop Ordered": true,
				"Seat Count":     1,
				"Laptop Budget":  2500.0,
				"Badge ID":       int64(42),
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "created", created.Output["action"])
	firstPageID := created.Output["page_id"].(string)

	// Re-executing with same RunID:StepKey replays from Postgres ledger cleanly.
	replayed, err := reg.Execute(ctx, notion.Type, connector.CallRequest{
		TenantID:  "spacelions",
		RunID:     "run_01jonboard",
		StepKey:   "tracker_initial",
		Operation: "upsert_db_row",
		Params:    map[string]any{"database": "onboarding_tracker"},
	})
	require.NoError(t, err)
	assert.True(t, replayed.Replayed)

	// Updating from a different step on the same run updates the exact same Notion row!
	updated, err := reg.Execute(ctx, notion.Type, connector.CallRequest{
		TenantID:  "spacelions",
		RunID:     "run_01jonboard",
		StepKey:   "tracker_closeout",
		Operation: "upsert_db_row",
		Params: map[string]any{
			"database": "onboarding_tracker",
			"properties": map[string]any{
				"Status": map[string]any{"status": map[string]any{"name": "Completed"}},
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "updated", updated.Output["action"])
	assert.Equal(t, firstPageID, updated.Output["page_id"])
	assert.Len(t, api.pages, 1, "repeated upserts for the same run_id must not create duplicates")

	// 4. query_db (RiskRead): returns the single upserted page, plus filter & page_size clamping.
	qRes, err := reg.Execute(ctx, notion.Type, connector.CallRequest{
		TenantID:  "spacelions",
		Operation: "query_db",
		Params: map[string]any{
			"database":  "onboarding_tracker",
			"page_size": 500,
			"filter": map[string]any{
				"property":  "Run ID",
				"rich_text": map[string]any{"equals": "run_01jonboard"},
			},
		},
	})
	require.NoError(t, err)
	results := qRes.Output["results"].([]map[string]any)
	require.Len(t, results, 1)
	assert.Equal(t, firstPageID, results[0]["id"])

	qClampLow, err := reg.Execute(ctx, notion.Type, connector.CallRequest{
		TenantID:  "spacelions",
		Operation: "query_db",
		Params: map[string]any{
			"database":  "onboarding_tracker",
			"page_size": float64(0),
		},
	})
	require.NoError(t, err)
	assert.NotNil(t, qClampLow.Output["results"])

	// 5. Validation & upstream error branches.
	conn, _, err := reg.Resolve(ctx, "spacelions", notion.Type)
	require.NoError(t, err)

	for _, badReq := range []connector.CallRequest{
		{Operation: "unknown_op"},
		{Operation: "upsert_db_row", Params: map[string]any{}},
		{Operation: "upsert_db_row", Params: map[string]any{"database": "unknown_alias", "run_id": "run_1"}},
		{Operation: "upsert_db_row", Params: map[string]any{"database": "onboarding_tracker"}},
		{Operation: "upsert_db_row", RunID: "run_1", Params: map[string]any{"database": "onboarding_tracker", "properties": "bad"}},
		{Operation: "upsert_db_row", RunID: "run_1", Params: map[string]any{"database": "onboarding_tracker", "properties": map[string]any{"Bad": []string{"x"}}}},
		{Operation: "query_db", Params: map[string]any{"database": "onboarding_tracker", "filter": "bad"}},
	} {
		_, err = conn.Call(ctx, badReq)
		require.Error(t, err)
	}

	api.queryStatus = http.StatusBadGateway
	_, err = conn.Call(ctx, connector.CallRequest{Operation: "query_db", Params: map[string]any{"database": "onboarding_tracker"}})
	require.Error(t, err)
	_, err = conn.Call(ctx, connector.CallRequest{RunID: "run_new", Operation: "upsert_db_row", Params: map[string]any{"database": "onboarding_tracker"}})
	require.Error(t, err)
	api.queryStatus = http.StatusOK

	api.createStatus = http.StatusBadGateway
	_, err = conn.Call(ctx, connector.CallRequest{RunID: "run_brand_new", Operation: "upsert_db_row", Params: map[string]any{"database": "onboarding_tracker"}})
	require.Error(t, err)
	api.createStatus = http.StatusOK

	api.patchStatus = http.StatusBadGateway
	_, err = conn.Call(ctx, connector.CallRequest{RunID: "run_01jonboard", Operation: "upsert_db_row", Params: map[string]any{"database": "onboarding_tracker"}})
	require.Error(t, err)
	api.patchStatus = http.StatusOK

	// Empty resource DB ID fails HealthCheck.
	require.NoError(t, connStore.Upsert(ctx, connectorstore.Record{
		TenantID:      "spacelions",
		ConnectorType: notion.Type,
		Enabled:       true,
		Settings:      map[string]string{"base_url": api.server.URL},
		Resources:     map[string]string{"empty_alias": "  "},
	}))
	badHealth, err = reg.CheckTenantHealth(ctx, "spacelions")
	require.NoError(t, err)
	assert.False(t, badHealth.Healthy())

	// Canceled context propagates HTTP transport errors cleanly.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	require.Error(t, conn.HealthCheck(canceledCtx))
	_, err = conn.Call(canceledCtx, connector.CallRequest{RunID: "run_x", Operation: "query_db", Params: map[string]any{"database": "onboarding_tracker"}})
	require.Error(t, err)
	_, err = conn.Call(canceledCtx, connector.CallRequest{RunID: "run_x", Operation: "upsert_db_row", Params: map[string]any{"database": "onboarding_tracker"}})
	require.Error(t, err)
}
