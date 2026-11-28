package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/spacelions/eunomia/internal/connector"
)

const (
	// Type is the canonical connector_type key for Slack (Socket Mode + channel/DM actions).
	Type = "slack"

	defaultBaseURL = "https://slack.com/api"
)

// ErrNotImplemented is returned by scaffolded Slack operations until full v0.2 Socket Mode execution lands.
var ErrNotImplemented = errors.New("slack operation scaffolded for v0.2")

// Connector scaffolds Slack Socket Mode health checks and channel/messaging capabilities.
type Connector struct {
	botToken   string
	appToken   string
	socketMode bool
	baseURL    string
	resources  map[string]string
	httpClient *http.Client
}

// Factory constructs a tenant-configured Slack connector from bot_token and (when Socket Mode is enabled) app_token.
func Factory(ctx context.Context, cfg connector.TenantConfig, secretReader connector.SecretReader) (connector.Connector, error) {
	return NewFactoryWithBaseURL(defaultBaseURL)(ctx, cfg, secretReader)
}

// NewFactoryWithBaseURL constructs a Slack Factory targeting an explicit API base URL
// (primarily for hermetic httptest verification without exposing URLs in tenant settings).
func NewFactoryWithBaseURL(baseURL string) connector.Factory {
	resolvedBase := defaultBaseURL
	if raw := strings.TrimRight(strings.TrimSpace(baseURL), "/"); raw != "" {
		resolvedBase = raw
	}

	return func(ctx context.Context, cfg connector.TenantConfig, secretReader connector.SecretReader) (connector.Connector, error) {
		botBytes, err := secretReader.Get(ctx, "bot_token")
		if err != nil {
			return nil, fmt.Errorf("slack missing bot_token secret: %w", err)
		}
		botToken := strings.TrimSpace(string(botBytes))
		if !strings.HasPrefix(botToken, "xoxb-") || len(botToken) <= len("xoxb-") {
			return nil, errors.New("slack bot_token must be a valid xoxb- token")
		}

		socketMode := !strings.EqualFold(strings.TrimSpace(cfg.Settings["socket_mode"]), "false")
		var appToken string
		if socketMode {
			appBytes, err := secretReader.Get(ctx, "app_token")
			if err != nil {
				return nil, fmt.Errorf("slack socket mode requires app_token secret: %w", err)
			}
			appToken = strings.TrimSpace(string(appBytes))
			if !strings.HasPrefix(appToken, "xapp-") || len(appToken) <= len("xapp-") {
				return nil, errors.New("slack app_token must be a valid xapp- token when socket_mode is enabled")
			}
		}

		return &Connector{
			botToken:   botToken,
			appToken:   appToken,
			socketMode: socketMode,
			baseURL:    resolvedBase,
			resources:  maps.Clone(cfg.Resources),
			httpClient: &http.Client{Timeout: 30 * time.Second},
		}, nil
	}
}

func (c *Connector) Type() string { return Type }

// SocketModeEnabled reports whether Slack Socket Mode is enabled for this tenant configuration.
func (c *Connector) SocketModeEnabled() bool { return c.socketMode }

func (c *Connector) Operations() []connector.OperationSpec {
	return []connector.OperationSpec{
		{
			Name:        "post_message",
			Description: "Post a message to a configured Slack channel alias or channel ID (scaffolded for v0.2).",
			RiskTier:    connector.RiskIrreversible,
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"channel", "text"},
				"properties": map[string]any{
					"channel": map[string]any{"type": "string"},
					"text":    map[string]any{"type": "string"},
				},
			},
		},
		{
			Name:        "invite_to_channel",
			Description: "Invite a Slack user ID to a configured channel (scaffolded for v0.2).",
			RiskTier:    connector.RiskWrite,
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"channel", "user_id"},
				"properties": map[string]any{
					"channel": map[string]any{"type": "string"},
					"user_id": map[string]any{"type": "string"},
				},
			},
		},
	}
}

// HealthCheck verifies the Slack bot token using `POST /auth.test`.
func (c *Connector) HealthCheck(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/auth.test", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.botToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("slack auth.test healthcheck failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reading slack auth.test response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack auth.test returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var authResp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &authResp); err != nil {
		return fmt.Errorf("decoding slack auth.test response: %w", err)
	}
	if !authResp.OK {
		return fmt.Errorf("slack auth.test failed: %s", authResp.Error)
	}
	return nil
}

func (c *Connector) Call(_ context.Context, req connector.CallRequest) (*connector.CallResult, error) {
	switch req.Operation {
	case "post_message":
		if _, err := c.resolveChannelTarget(req); err != nil {
			return nil, err
		}
		if stringParam(req.Params, "text") == "" {
			return nil, errors.New("slack.post_message requires text")
		}
		return nil, fmt.Errorf("slack.post_message: %w", ErrNotImplemented)

	case "invite_to_channel":
		if _, err := c.resolveChannelTarget(req); err != nil {
			return nil, err
		}
		if stringParam(req.Params, "user_id") == "" {
			return nil, errors.New("slack.invite_to_channel requires user_id")
		}
		return nil, fmt.Errorf("slack.invite_to_channel: %w", ErrNotImplemented)

	default:
		return nil, fmt.Errorf("%w: %s.%s", connector.ErrUnknownOperation, Type, req.Operation)
	}
}

// Deliver implements connector.Channel (scaffolded for slack_dm / slack_card in v0.2).
func (c *Connector) Deliver(_ context.Context, req connector.DeliveryRequest) (*connector.DeliveryReceipt, error) {
	if strings.TrimSpace(req.RecipientHandle) == "" {
		return nil, errors.New("slack delivery requires recipient_handle")
	}
	return nil, fmt.Errorf("slack delivery: %w", ErrNotImplemented)
}

func (c *Connector) resolveChannelTarget(req connector.CallRequest) (string, error) {
	raw := stringParam(req.Params, "channel")
	if raw == "" {
		return "", errors.New("channel is required")
	}
	if resolved := strings.TrimSpace(req.Resources[raw]); resolved != "" {
		return resolved, nil
	}
	if resolved := strings.TrimSpace(c.resources[raw]); resolved != "" {
		return resolved, nil
	}
	return raw, nil
}

func stringParam(params map[string]any, key string) string {
	v, _ := params[key].(string)
	return strings.TrimSpace(v)
}
