package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"connectrpc.com/grpchealth"
	"golang.org/x/net/http2"
)

func TestGRPCHealthCheck_ServingAndNotServing(t *testing.T) {
	var dbHealthy atomic.Bool
	dbHealthy.Store(true)

	handler := NewHandler(func(context.Context) error {
		if !dbHealthy.Load() {
			return errors.New("postgres down")
		}
		return nil
	})

	ts := httptest.NewUnstartedServer(handler)
	ts.EnableHTTP2 = true
	ts.Start()
	defer ts.Close()

	h2cClient := &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
	}

	endpoint := ts.URL + "/" + grpchealth.HealthV1ServiceName + "/Check"

	checkConnectJSON := func(wantStatus string) {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewReader([]byte(`{}`)))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := h2cClient.Do(req)
		if err != nil {
			t.Fatalf("connect health request failed: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected HTTP 200, got %d", resp.StatusCode)
		}
		var body struct {
			Status string `json:"status"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body.Status != wantStatus {
			t.Fatalf("expected status %q, got %q", wantStatus, body.Status)
		}
	}

	checkBinaryGRPC := func(wantEnum byte) {
		t.Helper()
		// Standard gRPC length-prefixed empty HealthCheckRequest: compression=0, length=0.
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewReader([]byte{0, 0, 0, 0, 0}))
		if err != nil {
			t.Fatalf("new grpc request: %v", err)
		}
		req.Header.Set("Content-Type", "application/grpc")
		resp, err := h2cClient.Do(req)
		if err != nil {
			t.Fatalf("grpc health request failed: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read grpc body: %v", err)
		}
		if got := resp.Trailer.Get("Grpc-Status"); got != "0" {
			t.Fatalf("expected Grpc-Status 0, got %q (headers=%v)", got, resp.Header)
		}
		// Protobuf HealthCheckResponse { ServingStatus status = 1; } -> 5-byte frame header + 0x08 <enum>
		wantFrame := []byte{0, 0, 0, 0, 2, 0x08, wantEnum}
		if !bytes.Equal(raw, wantFrame) {
			t.Fatalf("unexpected grpc frame: got %x, want %x", raw, wantFrame)
		}
	}

	// Healthy DB -> SERVING (enum 1)
	checkConnectJSON("SERVING_STATUS_SERVING")
	checkBinaryGRPC(1)

	// Unhealthy DB -> NOT_SERVING (enum 2)
	dbHealthy.Store(false)
	checkConnectJSON("SERVING_STATUS_NOT_SERVING")
	checkBinaryGRPC(2)
}

func TestNewHTTPServerAndNilReadiness(t *testing.T) {
	h := NewHandler(nil)
	srv := NewHTTPServer("127.0.0.1:0", h)
	if srv.Addr != "127.0.0.1:0" || srv.Handler == nil {
		t.Fatalf("unexpected server config: %+v", srv)
	}

	ts := httptest.NewUnstartedServer(h)
	ts.EnableHTTP2 = true
	ts.Start()
	defer ts.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/"+grpchealth.HealthV1ServiceName+"/Check", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with nil readiness check, got %d", resp.StatusCode)
	}
}
