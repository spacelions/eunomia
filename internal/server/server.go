package server

import (
	"context"
	"net/http"
	"time"

	"connectrpc.com/grpchealth"
	"connectrpc.com/grpcreflect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// ReadinessCheck verifies backing dependencies (e.g., PostgreSQL ping).
type ReadinessCheck func(ctx context.Context) error

// NewHandler builds the pure ConnectRPC (gRPC / h2c) handler with standard
// grpc.health.v1.Health readiness checks and gRPC reflection.
func NewHandler(readiness ReadinessCheck, extraServiceNames ...string) http.Handler {
	mux := http.NewServeMux()

	checker := &readinessChecker{check: readiness}
	mux.Handle(grpchealth.NewHandler(checker))

	serviceNames := append([]string{grpchealth.HealthV1ServiceName}, extraServiceNames...)
	reflector := grpcreflect.NewStaticReflector(serviceNames...)
	mux.Handle(grpcreflect.NewHandlerV1(reflector))
	mux.Handle(grpcreflect.NewHandlerV1Alpha(reflector))

	return h2c.NewHandler(mux, &http2.Server{})
}

// NewHTTPServer wraps the ConnectRPC h2c handler in an http.Server configured for graceful shutdown.
func NewHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
}

type readinessChecker struct {
	check ReadinessCheck
}

func (c *readinessChecker) Check(ctx context.Context, _ *grpchealth.CheckRequest) (*grpchealth.CheckResponse, error) {
	if c.check != nil {
		checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := c.check(checkCtx); err != nil {
			return &grpchealth.CheckResponse{Status: grpchealth.StatusNotServing}, nil
		}
	}
	return &grpchealth.CheckResponse{Status: grpchealth.StatusServing}, nil
}
