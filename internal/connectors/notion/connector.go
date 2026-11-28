package notion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/spacelions/eunomia/internal/connector"
)

const (
	// Type is the canonical connector_type key for Notion workspace & database operations.
	Type = "notion"

	defaultBaseURL       = "https://api.notion.com"
	defaultNotionVersion = "2022-06-28"
	defaultRunIDProperty = "Run ID"
)

// Connector implements Notion database query and idempotent tracker row upserts.
type Connector struct {
	token         string
	baseURL       string
	notionVersion string
	runIDProperty string
	resources     map[string]string
	httpClient    *http.Client
}

// Factory constructs a tenant-configured Notion connector using the encrypted `api_token` secret
// and tenant semantic resource aliases (e.g. onboarding_tracker -> database UUID).
func Factory(ctx context.Context, cfg connector.TenantConfig, secretReader connector.SecretReader) (connector.Connector, error) {
	tokenBytes, err := secretReader.Get(ctx, "api_token")
	if err != nil {
		return nil, fmt.Errorf("notion missing api_token secret: %w", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return nil, errors.New("notion api_token must not be empty")
	}

	baseURL := defaultBaseURL
	if raw := strings.TrimRight(strings.TrimSpace(cfg.Settings["base_url"]), "/"); raw != "" {
		baseURL = raw
	}
	notionVersion := defaultNotionVersion
	if raw := strings.TrimSpace(cfg.Settings["notion_version"]); raw != "" {
		notionVersion = raw
	}
	runIDProp := defaultRunIDProperty
	if raw := strings.TrimSpace(cfg.Settings["run_id_property"]); raw != "" {
		runIDProp = raw
	}

	return &Connector{
		token:         token,
		baseURL:       baseURL,
		notionVersion: notionVersion,
		runIDProperty: runIDProp,
		resources:     maps.Clone(cfg.Resources),
		httpClient:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (c *Connector) Type() string { return Type }

func (c *Connector) Operations() []connector.OperationSpec {
	return []connector.OperationSpec{
		{
			Name:        "upsert_db_row",
			Description: "Idempotently create or update a Notion database row keyed by run_id.",
			RiskTier:    connector.RiskWrite,
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"database"},
				"properties": map[string]any{
					"database":   map[string]any{"type": "string", "description": "Semantic resource alias for target database."},
					"run_id":     map[string]any{"type": "string"},
					"title":      map[string]any{"type": "string"},
					"properties": map[string]any{"type": "object"},
				},
			},
		},
		{
			Name:        "query_db",
			Description: "Query rows from a configured Notion database alias.",
			RiskTier:    connector.RiskRead,
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"database"},
				"properties": map[string]any{
					"database":  map[string]any{"type": "string"},
					"filter":    map[string]any{"type": "object"},
					"page_size": map[string]any{"type": "integer"},
				},
			},
		},
	}
}

// HealthCheck verifies the Notion integration token and checks read access to every configured database alias.
func (c *Connector) HealthCheck(ctx context.Context) error {
	status, body, err := c.doJSON(ctx, http.MethodGet, c.baseURL+"/v1/users/me", nil, nil)
	if err != nil {
		return fmt.Errorf("notion users/me healthcheck failed: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("notion authentication check returned %d: %s", status, truncateBody(body))
	}

	aliases := make([]string, 0, len(c.resources))
	for alias := range c.resources {
		aliases = append(aliases, alias)
	}
	slices.Sort(aliases)

	for _, alias := range aliases {
		dbID := strings.TrimSpace(c.resources[alias])
		if dbID == "" {
			return fmt.Errorf("notion resource alias %q has empty database_id", alias)
		}
		endpoint := fmt.Sprintf("%s/v1/databases/%s", c.baseURL, url.PathEscape(dbID))
		dbStatus, dbBody, dbErr := c.doJSON(ctx, http.MethodGet, endpoint, nil, nil)
		if dbErr != nil {
			return fmt.Errorf("notion healthcheck for alias %q (%s) failed: %w", alias, dbID, dbErr)
		}
		if dbStatus != http.StatusOK {
			return fmt.Errorf("notion database alias %q (%s) returned %d: %s", alias, dbID, dbStatus, truncateBody(dbBody))
		}
	}
	return nil
}

func (c *Connector) Call(ctx context.Context, req connector.CallRequest) (*connector.CallResult, error) {
	switch req.Operation {
	case "upsert_db_row":
		return c.upsertDBRow(ctx, req)
	case "query_db":
		return c.queryDB(ctx, req)
	default:
		return nil, fmt.Errorf("%w: %s.%s", connector.ErrUnknownOperation, Type, req.Operation)
	}
}

type notionPageSummary struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

type notionQueryResponse struct {
	Results []json.RawMessage `json:"results"`
	HasMore bool              `json:"has_more"`
}

func (c *Connector) upsertDBRow(ctx context.Context, req connector.CallRequest) (*connector.CallResult, error) {
	dbID, err := c.resolveDatabaseID(req)
	if err != nil {
		return nil, err
	}

	runID := stringParam(req.Params, "run_id")
	if runID == "" {
		runID = strings.TrimSpace(req.RunID)
	}
	if runID == "" {
		return nil, errors.New("upsert_db_row requires run_id")
	}

	props, err := c.buildProperties(req.Params, runID)
	if err != nil {
		return nil, err
	}

	queryPayload := map[string]any{
		"filter": map[string]any{
			"property": c.runIDProperty,
			"rich_text": map[string]any{
				"equals": runID,
			},
		},
		"page_size": 1,
	}

	var qResp notionQueryResponse
	queryURL := fmt.Sprintf("%s/v1/databases/%s/query", c.baseURL, url.PathEscape(dbID))
	status, body, err := c.doJSON(ctx, http.MethodPost, queryURL, queryPayload, &qResp)
	if err != nil {
		return nil, fmt.Errorf("querying notion database %q by run_id: %w", dbID, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("notion database query returned %d: %s", status, truncateBody(body))
	}

	if len(qResp.Results) > 0 {
		var existing notionPageSummary
		if err := json.Unmarshal(qResp.Results[0], &existing); err != nil {
			return nil, fmt.Errorf("decoding existing notion page: %w", err)
		}
		var updated notionPageSummary
		patchURL := fmt.Sprintf("%s/v1/pages/%s", c.baseURL, url.PathEscape(existing.ID))
		pStatus, pBody, pErr := c.doJSON(ctx, http.MethodPatch, patchURL, map[string]any{"properties": props}, &updated)
		if pErr != nil {
			return nil, fmt.Errorf("updating notion page %q: %w", existing.ID, pErr)
		}
		if pStatus != http.StatusOK {
			return nil, fmt.Errorf("notion page update returned %d: %s", pStatus, truncateBody(pBody))
		}
		return &connector.CallResult{
			Output: map[string]any{
				"page_id":     updated.ID,
				"url":         updated.URL,
				"action":      "updated",
				"database_id": dbID,
			},
		}, nil
	}

	createPayload := map[string]any{
		"parent": map[string]any{
			"database_id": dbID,
		},
		"properties": props,
	}
	var created notionPageSummary
	cStatus, cBody, cErr := c.doJSON(ctx, http.MethodPost, c.baseURL+"/v1/pages", createPayload, &created)
	if cErr != nil {
		return nil, fmt.Errorf("creating notion page in %q: %w", dbID, cErr)
	}
	if cStatus != http.StatusOK && cStatus != http.StatusCreated {
		return nil, fmt.Errorf("notion page create returned %d: %s", cStatus, truncateBody(cBody))
	}
	return &connector.CallResult{
		Output: map[string]any{
			"page_id":     created.ID,
			"url":         created.URL,
			"action":      "created",
			"database_id": dbID,
		},
	}, nil
}

func (c *Connector) queryDB(ctx context.Context, req connector.CallRequest) (*connector.CallResult, error) {
	dbID, err := c.resolveDatabaseID(req)
	if err != nil {
		return nil, err
	}

	pageSize := intParam(req.Params, "page_size", 10)
	if pageSize < 1 {
		pageSize = 1
	} else if pageSize > 100 {
		pageSize = 100
	}

	payload := map[string]any{
		"page_size": pageSize,
	}
	if rawFilter, ok := req.Params["filter"]; ok && rawFilter != nil {
		filterMap, ok := rawFilter.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("query_db filter must be an object, got %T", rawFilter)
		}
		payload["filter"] = filterMap
	}

	var out struct {
		Results []map[string]any `json:"results"`
		HasMore bool             `json:"has_more"`
	}
	endpoint := fmt.Sprintf("%s/v1/databases/%s/query", c.baseURL, url.PathEscape(dbID))
	status, body, err := c.doJSON(ctx, http.MethodPost, endpoint, payload, &out)
	if err != nil {
		return nil, fmt.Errorf("querying notion database %q: %w", dbID, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("notion query_db returned %d: %s", status, truncateBody(body))
	}
	if out.Results == nil {
		out.Results = []map[string]any{}
	}

	return &connector.CallResult{
		Output: map[string]any{
			"results":     out.Results,
			"has_more":    out.HasMore,
			"database_id": dbID,
		},
	}, nil
}

func (c *Connector) resolveDatabaseID(req connector.CallRequest) (string, error) {
	alias := stringParam(req.Params, "database")
	if alias == "" {
		return "", errors.New("database resource alias is required")
	}

	resources := req.Resources
	if len(resources) == 0 {
		resources = c.resources
	}
	dbID := strings.TrimSpace(resources[alias])
	if dbID == "" {
		return "", fmt.Errorf("unknown notion database alias %q", alias)
	}
	return dbID, nil
}

func (c *Connector) buildProperties(params map[string]any, runID string) (map[string]any, error) {
	props := make(map[string]any)

	if rawProps, ok := params["properties"]; ok && rawProps != nil {
		rawMap, ok := rawProps.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("properties must be an object, got %T", rawProps)
		}
		for k, v := range rawMap {
			converted, err := convertPropertyValue(v)
			if err != nil {
				return nil, fmt.Errorf("property %q: %w", k, err)
			}
			props[k] = converted
		}
	}

	if title := stringParam(params, "title"); title != "" {
		if _, exists := props["Name"]; !exists {
			props["Name"] = map[string]any{
				"title": []map[string]any{
					{"text": map[string]any{"content": title}},
				},
			}
		}
	}

	props[c.runIDProperty] = richTextValue(runID)
	return props, nil
}

func convertPropertyValue(v any) (any, error) {
	switch val := v.(type) {
	case map[string]any:
		return val, nil
	case string:
		return richTextValue(val), nil
	case bool:
		return map[string]any{"checkbox": val}, nil
	case int:
		return map[string]any{"number": float64(val)}, nil
	case int64:
		return map[string]any{"number": float64(val)}, nil
	case float64:
		return map[string]any{"number": val}, nil
	default:
		return nil, fmt.Errorf("unsupported property value type %T", v)
	}
}

func richTextValue(text string) map[string]any {
	return map[string]any{
		"rich_text": []map[string]any{
			{"text": map[string]any{"content": text}},
		},
	}
}

func (c *Connector) doJSON(ctx context.Context, method, targetURL string, payload, out any) (int, []byte, error) {
	var bodyReader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		bodyReader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, targetURL, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Notion-Version", c.notionVersion)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}

	if out != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return resp.StatusCode, respBody, fmt.Errorf("decoding response JSON: %w", err)
		}
	}
	return resp.StatusCode, respBody, nil
}

func stringParam(params map[string]any, key string) string {
	v, _ := params[key].(string)
	return strings.TrimSpace(v)
}

func intParam(params map[string]any, key string, fallback int) int {
	switch v := params[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	default:
		return fallback
	}
}

func truncateBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 256 {
		return s[:256] + "..."
	}
	return s
}
