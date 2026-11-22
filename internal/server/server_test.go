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
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := h2cClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var body struct {
			Status string `json:"status"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		assert.Equal(t, wantStatus, body.Status)
	}

	checkBinaryGRPC := func(wantEnum byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewReader([]byte{0, 0, 0, 0, 0}))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/grpc")
		resp, err := h2cClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, "0", resp.Trailer.Get("Grpc-Status"))

		wantFrame := []byte{0, 0, 0, 0, 2, 0x08, wantEnum}
		assert.Empty(t, cmp.Diff(wantFrame, raw))
	}

	checkConnectJSON("SERVING_STATUS_SERVING")
	checkBinaryGRPC(1)

	dbHealthy.Store(false)
	checkConnectJSON("SERVING_STATUS_NOT_SERVING")
	checkBinaryGRPC(2)
}

func TestNewHTTPServerAndNilReadiness(t *testing.T) {
	h := NewHandler(nil)
	srv := NewHTTPServer("127.0.0.1:0", h)
	assert.Equal(t, "127.0.0.1:0", srv.Addr)
	assert.NotNil(t, srv.Handler)

	ts := httptest.NewUnstartedServer(h)
	ts.EnableHTTP2 = true
	ts.Start()
	defer ts.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/"+grpchealth.HealthV1ServiceName+"/Check", bytes.NewReader([]byte(`{}`)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
