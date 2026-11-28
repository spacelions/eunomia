package googleworkspace

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/spacelions/eunomia/internal/actors"
	"github.com/spacelions/eunomia/internal/connector"
)

const (
	// Type is the canonical connector_type key for Google Workspace (Admin Directory + Gmail).
	Type = "google_workspace"

	defaultAdminBaseURL = "https://admin.googleapis.com"
	defaultGmailBaseURL = "https://gmail.googleapis.com"

	scopeAdminUsers    = "https://www.googleapis.com/auth/admin.directory.user"
	scopeGmailSend     = "https://www.googleapis.com/auth/gmail.send"
	scopeGmailReadonly = "https://www.googleapis.com/auth/gmail.readonly"

	idempotencyExternalIDType = "eunomia_idempotency_key"
	passwordAlphabet          = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789!@#$%^&*-_"
)

// Connector implements Google Workspace Directory & Gmail operations plus Channel delivery.
type Connector struct {
	domain           string
	impersonateEmail string
	adminBaseURL     string
	gmailBaseURL     string
	httpClient       *http.Client
}

// Factory constructs a tenant-configured Google Workspace connector authenticated via
// GCP Service Account JSON and Domain-Wide Delegation (impersonate_email).
func Factory(ctx context.Context, cfg connector.TenantConfig, secretReader connector.SecretReader) (connector.Connector, error) {
	domain := strings.ToLower(strings.TrimSpace(cfg.Settings["domain"]))
	if domain == "" {
		return nil, errors.New("google_workspace requires settings.domain")
	}

	impersonateEmail, err := actors.NormalizeEmail(cfg.Settings["impersonate_email"])
	if err != nil {
		return nil, fmt.Errorf("google_workspace invalid settings.impersonate_email: %w", err)
	}

	saJSON, err := secretReader.Get(ctx, "service_account_json")
	if err != nil {
		return nil, fmt.Errorf("google_workspace missing service_account_json secret: %w", err)
	}

	jwtCfg, err := google.JWTConfigFromJSON(saJSON, scopeAdminUsers, scopeGmailSend, scopeGmailReadonly)
	if err != nil {
		return nil, fmt.Errorf("google_workspace invalid service_account_json: %w", err)
	}
	jwtCfg.Subject = impersonateEmail
	if tokenURL := strings.TrimSpace(cfg.Settings["token_url"]); tokenURL != "" {
		jwtCfg.TokenURL = tokenURL
	}

	adminBase := defaultAdminBaseURL
	if raw := strings.TrimRight(strings.TrimSpace(cfg.Settings["admin_base_url"]), "/"); raw != "" {
		adminBase = raw
	}
	gmailBase := defaultGmailBaseURL
	if raw := strings.TrimRight(strings.TrimSpace(cfg.Settings["gmail_base_url"]), "/"); raw != "" {
		gmailBase = raw
	}

	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &oauth2.Transport{
			Source: jwtCfg.TokenSource(context.Background()),
			Base:   http.DefaultTransport,
		},
	}

	return &Connector{
		domain:           domain,
		impersonateEmail: impersonateEmail,
		adminBaseURL:     adminBase,
		gmailBaseURL:     gmailBase,
		httpClient:       httpClient,
	}, nil
}

func (c *Connector) Type() string { return Type }

func (c *Connector) Operations() []connector.OperationSpec {
	return []connector.OperationSpec{
		{
			Name:        "username_available",
			Description: "Check whether a candidate username or email address is available in Google Workspace.",
			RiskTier:    connector.RiskRead,
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"username_prefix": map[string]any{"type": "string"},
					"email":           map[string]any{"type": "string"},
				},
			},
		},
		{
			Name:        "create_user",
			Description: "Provision a new Google Workspace user idempotently using externalIds tracking.",
			RiskTier:    connector.RiskWrite,
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"given_name", "family_name"},
				"properties": map[string]any{
					"primary_email":                 map[string]any{"type": "string"},
					"username_prefix":               map[string]any{"type": "string"},
					"given_name":                    map[string]any{"type": "string"},
					"family_name":                   map[string]any{"type": "string"},
					"password":                      map[string]any{"type": "string"},
					"org_unit_path":                 map[string]any{"type": "string"},
					"change_password_at_next_login": map[string]any{"type": "boolean"},
				},
			},
		},
		{
			Name:        "send_email",
			Description: "Send an email via Gmail on behalf of the delegated workspace sender.",
			RiskTier:    connector.RiskIrreversible,
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"to", "subject", "body"},
				"properties": map[string]any{
					"to":       map[string]any{"type": "string"},
					"subject":  map[string]any{"type": "string"},
					"body":     map[string]any{"type": "string"},
					"cc":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"reply_to": map[string]any{"type": "string"},
				},
			},
		},
	}
}

// HealthCheck verifies both Admin Directory user lookup and Gmail profile access.
func (c *Connector) HealthCheck(ctx context.Context) error {
	adminURL := fmt.Sprintf("%s/admin/directory/v1/users?domain=%s&maxResults=1",
		c.adminBaseURL, url.QueryEscape(c.domain))
	status, body, err := c.doJSON(ctx, http.MethodGet, adminURL, nil, nil)
	if err != nil {
		return fmt.Errorf("google_workspace admin.directory healthcheck request failed: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("google_workspace admin.directory scope check returned %d: %s", status, truncateBody(body))
	}

	gmailURL := fmt.Sprintf("%s/gmail/v1/users/me/profile", c.gmailBaseURL)
	status, body, err = c.doJSON(ctx, http.MethodGet, gmailURL, nil, nil)
	if err != nil {
		return fmt.Errorf("google_workspace gmail healthcheck request failed: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("google_workspace gmail scope check returned %d: %s", status, truncateBody(body))
	}
	return nil
}

func (c *Connector) Call(ctx context.Context, req connector.CallRequest) (*connector.CallResult, error) {
	switch req.Operation {
	case "username_available":
		return c.usernameAvailable(ctx, req.Params)
	case "create_user":
		return c.createUser(ctx, req.IdempotencyKey, req.Params)
	case "send_email":
		return c.sendEmailOp(ctx, req.Params)
	default:
		return nil, fmt.Errorf("%w: %s.%s", connector.ErrUnknownOperation, Type, req.Operation)
	}
}

// Deliver implements connector.Channel to send Magic Links and approval prompts via Gmail.
func (c *Connector) Deliver(ctx context.Context, req connector.DeliveryRequest) (*connector.DeliveryReceipt, error) {
	body := req.Body
	if link := strings.TrimSpace(req.ActionURL); link != "" && !strings.Contains(body, link) {
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		if body != "" {
			body += "\n"
		}
		body += "Action link: " + link
	}
	subject := strings.TrimSpace(req.Subject)
	if subject == "" {
		subject = "Action required"
	}

	output, err := c.sendEmail(ctx, req.RecipientEmail, subject, body, nil, "")
	if err != nil {
		return nil, err
	}
	return &connector.DeliveryReceipt{
		Provider: Type,
		Meta:     output,
	}, nil
}

func (c *Connector) usernameAvailable(ctx context.Context, params map[string]any) (*connector.CallResult, error) {
	email, err := c.resolveEmail(params)
	if err != nil {
		return nil, err
	}

	var out struct {
		ID string `json:"id"`
	}
	endpoint := fmt.Sprintf("%s/admin/directory/v1/users/%s", c.adminBaseURL, url.PathEscape(email))
	status, body, err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &out)
	if err != nil {
		return nil, fmt.Errorf("checking username %q: %w", email, err)
	}

	switch status {
	case http.StatusNotFound:
		return &connector.CallResult{
			Output: map[string]any{
				"available": true,
				"email":     email,
			},
		}, nil
	case http.StatusOK:
		return &connector.CallResult{
			Output: map[string]any{
				"available": false,
				"email":     email,
			},
		}, nil
	default:
		return nil, fmt.Errorf("admin directory lookup for %q returned %d: %s", email, status, truncateBody(body))
	}
}

type adminUserRecord struct {
	ID           string `json:"id"`
	PrimaryEmail string `json:"primaryEmail"`
	ExternalIDs  []struct {
		Type       string `json:"type"`
		CustomType string `json:"customType"`
		Value      string `json:"value"`
	} `json:"externalIds"`
}

func (c *Connector) createUser(ctx context.Context, idempotencyKey string, params map[string]any) (*connector.CallResult, error) {
	primaryEmail, err := c.resolveEmail(params)
	if err != nil {
		return nil, err
	}
	givenName := stringParam(params, "given_name")
	familyName := stringParam(params, "family_name")
	if givenName == "" || familyName == "" {
		return nil, errors.New("create_user requires given_name and family_name")
	}

	password := stringParam(params, "password")
	if password == "" {
		password, err = generateTempPassword(20)
		if err != nil {
			return nil, fmt.Errorf("generating temporary password: %w", err)
		}
	}

	orgUnitPath := stringParam(params, "org_unit_path")
	if orgUnitPath == "" {
		orgUnitPath = "/"
	}
	changePassword := true
	if v, ok := params["change_password_at_next_login"].(bool); ok {
		changePassword = v
	}

	payload := map[string]any{
		"primaryEmail":              primaryEmail,
		"name":                      map[string]any{"givenName": givenName, "familyName": familyName},
		"password":                  password,
		"changePasswordAtNextLogin": changePassword,
		"orgUnitPath":               orgUnitPath,
	}
	idemKey := strings.TrimSpace(idempotencyKey)
	if idemKey != "" {
		payload["externalIds"] = []map[string]any{
			{
				"type":       "custom",
				"customType": idempotencyExternalIDType,
				"value":      idemKey,
			},
		}
	}

	var created adminUserRecord
	endpoint := fmt.Sprintf("%s/admin/directory/v1/users", c.adminBaseURL)
	status, body, err := c.doJSON(ctx, http.MethodPost, endpoint, payload, &created)
	if err != nil {
		return nil, fmt.Errorf("creating google workspace user %q: %w", primaryEmail, err)
	}

	if status == http.StatusOK || status == http.StatusCreated {
		resolvedEmail := created.PrimaryEmail
		if resolvedEmail == "" {
			resolvedEmail = primaryEmail
		}
		return &connector.CallResult{
			Output: map[string]any{
				"user_id":         created.ID,
				"primary_email":   strings.ToLower(resolvedEmail),
				"already_existed": false,
				"temp_password":   password,
			},
		}, nil
	}

	if status == http.StatusConflict {
		if idemKey == "" {
			return nil, fmt.Errorf("google_workspace user %q already exists", primaryEmail)
		}
		var existing adminUserRecord
		getURL := fmt.Sprintf("%s/admin/directory/v1/users/%s", c.adminBaseURL, url.PathEscape(primaryEmail))
		getStatus, getBody, getErr := c.doJSON(ctx, http.MethodGet, getURL, nil, &existing)
		if getErr != nil {
			return nil, fmt.Errorf("verifying existing user %q after 409: %w", primaryEmail, getErr)
		}
		if getStatus != http.StatusOK {
			return nil, fmt.Errorf("fetching existing user %q after 409 returned %d: %s", primaryEmail, getStatus, truncateBody(getBody))
		}
		if hasMatchingIdempotencyKey(existing, idemKey) {
			resolvedEmail := existing.PrimaryEmail
			if resolvedEmail == "" {
				resolvedEmail = primaryEmail
			}
			return &connector.CallResult{
				Output: map[string]any{
					"user_id":         existing.ID,
					"primary_email":   strings.ToLower(resolvedEmail),
					"already_existed": true,
				},
			}, nil
		}
		return nil, fmt.Errorf("google_workspace user %q already exists without matching idempotency_key", primaryEmail)
	}

	return nil, fmt.Errorf("google_workspace create_user returned %d: %s", status, truncateBody(body))
}

func hasMatchingIdempotencyKey(rec adminUserRecord, idemKey string) bool {
	for _, ext := range rec.ExternalIDs {
		if ext.CustomType == idempotencyExternalIDType && ext.Value == idemKey {
			return true
		}
	}
	return false
}

func (c *Connector) sendEmailOp(ctx context.Context, params map[string]any) (*connector.CallResult, error) {
	to := stringParam(params, "to")
	subject := stringParam(params, "subject")
	body := stringParam(params, "body")
	replyTo := stringParam(params, "reply_to")
	cc, err := parseEmailList(params["cc"])
	if err != nil {
		return nil, err
	}

	output, err := c.sendEmail(ctx, to, subject, body, cc, replyTo)
	if err != nil {
		return nil, err
	}
	return &connector.CallResult{Output: output}, nil
}

func (c *Connector) sendEmail(ctx context.Context, to, subject, body string, cc []string, replyTo string) (map[string]any, error) {
	normTo, err := actors.NormalizeEmail(to)
	if err != nil {
		return nil, fmt.Errorf("invalid email recipient %q: %w", to, err)
	}
	if strings.TrimSpace(subject) == "" {
		return nil, errors.New("email subject is required")
	}
	if strings.TrimSpace(body) == "" {
		return nil, errors.New("email body is required")
	}
	if containsHeaderBreak(subject) || containsHeaderBreak(replyTo) || containsHeaderBreak(normTo) {
		return nil, errors.New("email headers must not contain CR or LF")
	}

	normCC := make([]string, 0, len(cc))
	for _, addr := range cc {
		n, err := actors.NormalizeEmail(addr)
		if err != nil {
			return nil, fmt.Errorf("invalid cc address %q: %w", addr, err)
		}
		normCC = append(normCC, n)
	}

	var normReplyTo string
	if strings.TrimSpace(replyTo) != "" {
		normReplyTo, err = actors.NormalizeEmail(replyTo)
		if err != nil {
			return nil, fmt.Errorf("invalid reply_to address %q: %w", replyTo, err)
		}
	}

	var msg strings.Builder
	msg.WriteString("From: " + c.impersonateEmail + "\r\n")
	msg.WriteString("To: " + normTo + "\r\n")
	if len(normCC) > 0 {
		msg.WriteString("Cc: " + strings.Join(normCC, ", ") + "\r\n")
	}
	if normReplyTo != "" {
		msg.WriteString("Reply-To: " + normReplyTo + "\r\n")
	}
	msg.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", subject) + "\r\n")
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")
	msg.WriteString("\r\n")
	msg.WriteString(body)

	rawEncoded := base64.RawURLEncoding.EncodeToString([]byte(msg.String()))
	var sent struct {
		ID       string `json:"id"`
		ThreadID string `json:"threadId"`
	}
	endpoint := fmt.Sprintf("%s/gmail/v1/users/me/messages/send", c.gmailBaseURL)
	status, respBody, err := c.doJSON(ctx, http.MethodPost, endpoint, map[string]any{"raw": rawEncoded}, &sent)
	if err != nil {
		return nil, fmt.Errorf("sending gmail message to %q: %w", normTo, err)
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return nil, fmt.Errorf("gmail send returned %d: %s", status, truncateBody(respBody))
	}

	return map[string]any{
		"message_id": sent.ID,
		"thread_id":  sent.ThreadID,
		"to":         normTo,
	}, nil
}

func (c *Connector) resolveEmail(params map[string]any) (string, error) {
	for _, key := range []string{"primary_email", "email"} {
		if raw := stringParam(params, key); raw != "" {
			norm, err := actors.NormalizeEmail(raw)
			if err != nil {
				return "", err
			}
			return norm, nil
		}
	}
	for _, key := range []string{"username_prefix", "desired_prefix", "prefix"} {
		if prefix := strings.ToLower(stringParam(params, key)); prefix != "" {
			if strings.Contains(prefix, "@") {
				return "", fmt.Errorf("username prefix %q must not contain '@'", prefix)
			}
			return actors.NormalizeEmail(prefix + "@" + c.domain)
		}
	}
	return "", errors.New("email/primary_email or username_prefix is required")
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

func generateTempPassword(length int) (string, error) {
	buf := make([]byte, length)
	max := big.NewInt(int64(len(passwordAlphabet)))
	for i := range buf {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		buf[i] = passwordAlphabet[n.Int64()]
	}
	return string(buf), nil
}

func stringParam(params map[string]any, key string) string {
	v, _ := params[key].(string)
	return strings.TrimSpace(v)
}

func parseEmailList(raw any) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, nil
		}
		parts := strings.Split(v, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if strings.TrimSpace(p) != "" {
				out = append(out, strings.TrimSpace(p))
			}
		}
		return out, nil
	case []string:
		return v, nil
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("expected string email in list, got %T", item)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("invalid email list type %T", raw)
	}
}

func containsHeaderBreak(s string) bool {
	return strings.ContainsAny(s, "\r\n")
}

func truncateBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 256 {
		return s[:256] + "..."
	}
	return s
}
