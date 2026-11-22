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
	if err != nil {
		t.Fatalf("secret keygen failed: %v", err)
	}
	trimmed := strings.TrimSpace(out)
	if !strings.HasPrefix(trimmed, "v2:") {
		t.Fatalf("expected v2: prefix, got %q", trimmed)
	}
	if _, err := secrets.ParseLocalKeyWrapper(trimmed); err != nil {
		t.Fatalf("generated key failed to parse: %v", err)
	}

	if _, err := execCLI(context.Background(), "", "secret", "keygen", "--id", "bad:id"); err == nil {
		t.Fatal("expected error for invalid key id")
	}
}

func TestParseKVPairs(t *testing.T) {
	got, err := parseKVPairs([]string{"domain=spacelions.com", "impersonate_email=ops@spacelions.com"})
	if err != nil {
		t.Fatalf("parseKVPairs: %v", err)
	}
	if got["domain"] != "spacelions.com" || got["impersonate_email"] != "ops@spacelions.com" {
		t.Fatalf("unexpected map: %+v", got)
	}

	if _, err := parseKVPairs([]string{"invalid-no-equals"}); err == nil {
		t.Fatal("expected error for missing =")
	}
}

func TestCLI_Migrate_Connector_Secret_AndServeLifecycle(t *testing.T) {
	dsn, _ := testpg.NewRaw(t)
	ctx := context.Background()

	k1, err := secrets.GenerateLocalKey("v1")
	if err != nil {
		t.Fatalf("GenerateLocalKey v1: %v", err)
	}
	k2, err := secrets.GenerateLocalKey("v2")
	if err != nil {
		t.Fatalf("GenerateLocalKey v2: %v", err)
	}

	t.Setenv("EUNOMIA_DATABASE_URL", dsn)
	t.Setenv("EUNOMIA_MASTER_KEYS", k1)
	t.Setenv("EUNOMIA_DEFAULT_TENANT", "spacelions")

	// 0. Serve must fail fast before migrations are run (never auto-migrates).
	if _, err := execCLI(ctx, "", "serve"); err == nil {
		t.Fatal("expected serve to fail fast on unmigrated database")
	}

	// 1. Migrate (first time applies, second time up to date).
	if _, err := execCLI(ctx, "", "migrate"); err != nil {
		t.Fatalf("migrate first run failed: %v", err)
	}
	if _, err := execCLI(ctx, "", "migrate"); err != nil {
		t.Fatalf("migrate second run failed: %v", err)
	}

	// 2. Connector set (create + merge), list, disable, and error checks.
	if _, err := execCLI(ctx, "", "connector", "set"); err == nil {
		t.Fatal("expected error when --type is missing")
	}
	if _, err := execCLI(ctx, "", "connector", "set", "--type", "notion", "--setting", "bad"); err == nil {
		t.Fatal("expected error for invalid --setting")
	}
	if _, err := execCLI(ctx, "", "connector", "set", "--type", "notion", "--resource", "bad"); err == nil {
		t.Fatal("expected error for invalid --resource")
	}

	out, err := execCLI(ctx, "", "connector", "set",
		"--type", "notion",
		"--enabled=true",
		"--setting", "workspace=spacelions",
		"--resource", "onboarding_tracker=db-1",
	)
	if err != nil || !strings.Contains(out, "Saved connector spacelions/notion") {
		t.Fatalf("connector set failed: err=%v out=%q", err, out)
	}

	// Merge an additional setting on existing connector.
	if _, err := execCLI(ctx, "", "connector", "set",
		"--type", "notion",
		"--enabled=true",
		"--setting", "region=us",
	); err != nil {
		t.Fatalf("connector set merge failed: %v", err)
	}

	listOut, err := execCLI(ctx, "", "connector", "list")
	if err != nil {
		t.Fatalf("connector list failed: %v", err)
	}
	if !strings.Contains(listOut, "notion") || !strings.Contains(listOut, "onboarding_tracker") || !strings.Contains(listOut, "region") {
		t.Fatalf("unexpected connector list output:\n%s", listOut)
	}

	if _, err := execCLI(ctx, "", "connector", "disable"); err == nil {
		t.Fatal("expected error when --type is missing on disable")
	}
	if _, err := execCLI(ctx, "", "connector", "disable", "--type", "nonexistent"); err == nil {
		t.Fatal("expected error when disabling missing connector")
	}

	// 3. Secret set (stdin + file + raw + errors), list, rotate-master-key, delete.
	if _, err := execCLI(ctx, "tok", "secret", "set"); err == nil {
		t.Fatal("expected error when --connector/--key missing")
	}
	if _, err := execCLI(ctx, "", "secret", "set", "--connector", "notion", "--key", "token"); err == nil {
		t.Fatal("expected error when secret value is empty")
	}
	if _, err := execCLI(ctx, "", "secret", "set", "--connector", "notion", "--key", "token", "--file", "/no/such/file"); err == nil {
		t.Fatal("expected error when --file does not exist")
	}

	if _, err := execCLI(ctx, "secret-token-value\n", "secret", "set", "--connector", "notion", "--key", "token"); err != nil {
		t.Fatalf("secret set from stdin failed: %v", err)
	}

	saFile := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(saFile, []byte(`{"type":"service_account"}`), 0o600); err != nil {
		t.Fatalf("write temp sa file: %v", err)
	}
	if _, err := execCLI(ctx, "", "secret", "set", "--connector", "google_workspace", "--key", "sa_json", "--file", saFile); err != nil {
		t.Fatalf("secret set from --file failed: %v", err)
	}

	secList, err := execCLI(ctx, "", "secret", "list")
	if err != nil {
		t.Fatalf("secret list failed: %v", err)
	}
	if !strings.Contains(secList, "notion") || !strings.Contains(secList, "v1") {
		t.Fatalf("unexpected secret list:\n%s", secList)
	}
	if strings.Contains(secList, "secret-token-value") {
		t.Fatalf("secret list leaked plaintext:\n%s", secList)
	}

	// Rotate master key with missing old key fails cleanly.
	k3, _ := secrets.GenerateLocalKey("v3")
	t.Setenv("EUNOMIA_MASTER_KEYS", k3)
	if _, err := execCLI(ctx, "", "secret", "rotate-master-key"); err == nil {
		t.Fatal("expected rotate-master-key error when v1 is missing")
	}

	// Rotate master key to v2.
	t.Setenv("EUNOMIA_MASTER_KEYS", k2+","+k1)
	rotOut, err := execCLI(ctx, "", "secret", "rotate-master-key")
	if err != nil || !strings.Contains(rotOut, "Re-wrapped 2 secret(s)") {
		t.Fatalf("rotate-master-key failed: err=%v out=%q", err, rotOut)
	}

	// Retire v1 and delete secrets.
	t.Setenv("EUNOMIA_MASTER_KEYS", k2)
	if _, err := execCLI(ctx, "", "secret", "delete"); err == nil {
		t.Fatal("expected error when --connector/--key missing on delete")
	}
	if _, err := execCLI(ctx, "", "secret", "delete", "--connector", "notion", "--key", "token"); err != nil {
		t.Fatalf("secret delete failed: %v", err)
	}
	if _, err := execCLI(ctx, "", "secret", "delete", "--connector", "notion", "--key", "token"); err == nil {
		t.Fatal("expected error deleting already-deleted secret")
	}

	// 4. Serve lifecycle: start ConnectRPC server on free port, verify gRPC health check, cancel cleanly.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
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
	deadline := time.Now().Add(5 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, healthURL, bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		resp, reqErr := h2cClient.Do(req)
		if reqErr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancelServe()
	if !ready {
		t.Fatal("ConnectRPC server did not become healthy in time")
	}
	if sErr := <-serveErrCh; sErr != nil {
		t.Fatalf("serve returned error on graceful shutdown: %v", sErr)
	}

	// Also verify serve fails cleanly when grpc-addr is already bound.
	busyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind busy listener: %v", err)
	}
	defer func() { _ = busyLn.Close() }()
	if _, err := execCLI(ctx, "", "serve", "--grpc-addr", busyLn.Addr().String()); err == nil {
		t.Fatal("expected serve error when grpc-addr is already in use")
	}

	// Also verify default root invocation runs serve (fails fast with bad config).
	t.Setenv("EUNOMIA_MASTER_KEYS", "")
	if _, err := execCLI(ctx, ""); err == nil {
		t.Fatal("expected fail-fast when EUNOMIA_MASTER_KEYS is unset")
	}

	// Disable connector at end.
	if _, err := execCLI(ctx, "", "connector", "disable", "--type", "notion"); err != nil {
		t.Fatalf("connector disable failed: %v", err)
	}
}

func TestCLI_BadConfigAndUnreachableDBErrors(t *testing.T) {
	ctx := context.Background()
	k1, _ := secrets.GenerateLocalKey("v1")

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
		if _, err := execCLI(ctx, "v", args...); err == nil {
			t.Errorf("expected config error for %v", args)
		}
	}

	// Valid config syntax but unreachable Postgres -> all DB commands fail fast.
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
		if _, err := execCLI(ctx, "v", args...); err == nil {
			t.Errorf("expected unreachable DB error for %v", args)
		}
	}
}
