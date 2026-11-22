package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/grpchealth"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"

	"github.com/spacelions/eunomia/internal/secrets"
	"github.com/spacelions/eunomia/internal/testpg"
)

func execCLI(ctx context.Context, stdin string, args ...string) (string, error) {
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(ctx)
	return out.String(), err
}

func TestSecretKeygenCommand(t *testing.T) {
	out, err := execCLI(context.Background(), "", "secret", "keygen", "--id", "v2")
	require.NoError(t, err)

	trimmed := strings.TrimSpace(out)
	assert.True(t, strings.HasPrefix(trimmed, "v2:"))
	_, err = secrets.ParseLocalKeyWrapper(trimmed)
	require.NoError(t, err)

	_, err = execCLI(context.Background(), "", "secret", "keygen", "--id", "bad:id")
	require.Error(t, err)
}

func TestParseKVPairs(t *testing.T) {
	got, err := parseKVPairs([]string{"domain=spacelions.com", "impersonate_email=ops@spacelions.com"})
	require.NoError(t, err)
	want := map[string]string{
		"domain":            "spacelions.com",
		"impersonate_email": "ops@spacelions.com",
	}
	assert.Empty(t, cmp.Diff(want, got))

	_, err = parseKVPairs([]string{"invalid-no-equals"})
	require.Error(t, err)
}

func TestCLI_Migrate_Connector_Secret_AndServeLifecycle(t *testing.T) {
	dsn, _ := testpg.NewRaw(t)
	ctx := context.Background()

	k1, err := secrets.GenerateLocalKey("v1")
	require.NoError(t, err)
	k2, err := secrets.GenerateLocalKey("v2")
	require.NoError(t, err)
	k3, err := secrets.GenerateLocalKey("v3")
	require.NoError(t, err)

	t.Setenv("EUNOMIA_DATABASE_URL", dsn)
	t.Setenv("EUNOMIA_MASTER_KEYS", k1)
	t.Setenv("EUNOMIA_DEFAULT_TENANT", "spacelions")

	// 0. Serve must fail fast before migrations are run (never auto-migrates).
	_, err = execCLI(ctx, "", "serve")
	require.Error(t, err)

	// 1. Migrate (first time applies, second time up to date).
	_, err = execCLI(ctx, "", "migrate")
	require.NoError(t, err)
	_, err = execCLI(ctx, "", "migrate")
	require.NoError(t, err)

	// 2. Connector set (create + merge), list, disable, and error checks.
	_, err = execCLI(ctx, "", "connector", "set")
	require.Error(t, err)
	_, err = execCLI(ctx, "", "connector", "set", "--type", "notion", "--setting", "bad")
	require.Error(t, err)
	_, err = execCLI(ctx, "", "connector", "set", "--type", "notion", "--resource", "bad")
	require.Error(t, err)

	out, err := execCLI(ctx, "", "connector", "set",
		"--type", "notion",
		"--enabled=true",
		"--setting", "workspace=spacelions",
		"--resource", "onboarding_tracker=db-1",
	)
	require.NoError(t, err)
	assert.Contains(t, out, "Saved connector spacelions/notion")

	_, err = execCLI(ctx, "", "connector", "set",
		"--type", "notion",
		"--enabled=true",
		"--setting", "region=us",
	)
	require.NoError(t, err)

	listOut, err := execCLI(ctx, "", "connector", "list")
	require.NoError(t, err)
	assert.Contains(t, listOut, "notion")
	assert.Contains(t, listOut, "onboarding_tracker")
	assert.Contains(t, listOut, "region")

	_, err = execCLI(ctx, "", "connector", "disable")
	require.Error(t, err)
	_, err = execCLI(ctx, "", "connector", "disable", "--type", "nonexistent")
	require.Error(t, err)

	// 3. Secret set (stdin + file + errors), list, rotate-master-key, delete.
	_, err = execCLI(ctx, "tok", "secret", "set")
	require.Error(t, err)
	_, err = execCLI(ctx, "", "secret", "set", "--connector", "notion", "--key", "token")
	require.Error(t, err)
	_, err = execCLI(ctx, "", "secret", "set", "--connector", "notion", "--key", "token", "--file", "/no/such/file")
	require.Error(t, err)

	_, err = execCLI(ctx, "secret-token-value\n", "secret", "set", "--connector", "notion", "--key", "token")
	require.NoError(t, err)

	saFile := filepath.Join(t.TempDir(), "sa.json")
	require.NoError(t, os.WriteFile(saFile, []byte(`{"type":"service_account"}`), 0o600))
	_, err = execCLI(ctx, "", "secret", "set", "--connector", "google_workspace", "--key", "sa_json", "--file", saFile)
	require.NoError(t, err)

	secList, err := execCLI(ctx, "", "secret", "list")
	require.NoError(t, err)
	assert.Contains(t, secList, "notion")
	assert.Contains(t, secList, "v1")
	assert.NotContains(t, secList, "secret-token-value")

	// Rotate master key with missing old key fails cleanly.
	t.Setenv("EUNOMIA_MASTER_KEYS", k3)
	_, err = execCLI(ctx, "", "secret", "rotate-master-key")
	require.Error(t, err)

	// Rotate master key to v2.
	t.Setenv("EUNOMIA_MASTER_KEYS", k2+","+k1)
	rotOut, err := execCLI(ctx, "", "secret", "rotate-master-key")
	require.NoError(t, err)
	assert.Contains(t, rotOut, "Re-wrapped 2 secret(s)")

	// Retire v1 and delete secrets.
	t.Setenv("EUNOMIA_MASTER_KEYS", k2)
	_, err = execCLI(ctx, "", "secret", "delete")
	require.Error(t, err)
	_, err = execCLI(ctx, "", "secret", "delete", "--connector", "notion", "--key", "token")
	require.NoError(t, err)
	_, err = execCLI(ctx, "", "secret", "delete", "--connector", "notion", "--key", "token")
	require.Error(t, err)

	// 4. Serve lifecycle: start ConnectRPC server on free port, verify gRPC health check, cancel cleanly.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcAddr := ln.Addr().String()
	_ = ln.Close()

	serveCtx, cancelServe := context.WithCancel(ctx)
	serveErrCh := make(chan error, 1)
	go func() {
		_, sErr := execCLI(serveCtx, "", "serve", "--grpc-addr", grpcAddr)
		serveErrCh <- sErr
	}()

	h2cClient := &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(dCtx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(dCtx, network, addr)
			},
		},
	}

	healthURL := fmt.Sprintf("http://%s/%s/Check", grpcAddr, grpchealth.HealthV1ServiceName)
	require.Eventually(t, func() bool {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, healthURL, bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		resp, reqErr := h2cClient.Do(req)
		if reqErr != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 50*time.Millisecond, "ConnectRPC server did not become healthy in time")

	cancelServe()
	require.NoError(t, <-serveErrCh)

	// Serve fails cleanly when grpc-addr is already bound.
	busyLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = busyLn.Close() }()
	_, err = execCLI(ctx, "", "serve", "--grpc-addr", busyLn.Addr().String())
	require.Error(t, err)

	// Default root invocation runs serve (fails fast with bad config).
	t.Setenv("EUNOMIA_MASTER_KEYS", "")
	_, err = execCLI(ctx, "")
	require.Error(t, err)

	_, err = execCLI(ctx, "", "connector", "disable", "--type", "notion")
	require.NoError(t, err)
}

func TestCLI_BadConfigAndUnreachableDBErrors(t *testing.T) {
	ctx := context.Background()
	k1, err := secrets.GenerateLocalKey("v1")
	require.NoError(t, err)

	t.Setenv("EUNOMIA_DATABASE_URL", "")
	t.Setenv("EUNOMIA_MASTER_KEYS", "")

	for _, args := range [][]string{
		{"migrate"},
		{"serve"},
		{"secret", "set", "--connector", "notion", "--key", "token"},
		{"secret", "list"},
		{"secret", "delete", "--connector", "notion", "--key", "token"},
		{"secret", "rotate-master-key"},
		{"connector", "set", "--type", "notion"},
		{"connector", "list"},
		{"connector", "disable", "--type", "notion"},
	} {
		_, err := execCLI(ctx, "v", args...)
		assert.Error(t, err, "expected config error for %v", args)
	}

	t.Setenv("EUNOMIA_DATABASE_URL", "postgres://postgres@127.0.0.1:1/eunomia?sslmode=disable")
	t.Setenv("EUNOMIA_MASTER_KEYS", k1)
	for _, args := range [][]string{
		{"migrate"},
		{"serve"},
		{"secret", "list"},
		{"connector", "set", "--type", "notion"},
		{"connector", "list"},
		{"connector", "disable", "--type", "notion"},
	} {
		_, err := execCLI(ctx, "v", args...)
		assert.Error(t, err, "expected unreachable DB error for %v", args)
	}
}
