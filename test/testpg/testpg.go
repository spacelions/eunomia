package testpg

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/spacelions/eunomia/internal/db"
)

// NewRaw starts an ephemeral PostgreSQL instance on a free local port with an empty `eunomia` database.
// Requires `initdb` and `postgres` on PATH (provided by `nix develop`).
func NewRaw(t *testing.T) (dsn string, database *bun.DB) {
	t.Helper()

	if _, err := exec.LookPath("postgres"); err != nil {
		t.Fatalf("postgres binary not found on PATH (run tests inside `nix develop`): %v", err)
	}

	port := freePort(t)
	baseDir := t.TempDir()
	pgData := filepath.Join(baseDir, "pgdata")

	initCmd := exec.Command("initdb",
		"-D", pgData,
		"-U", "postgres",
		"--auth=trust",
		"--encoding=UTF8",
		"--locale=C",
		"--no-instructions",
		"--nosync",
	)
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("initdb failed: %v\n%s", err, out)
	}

	var pgLogs lockedBuffer
	pgCmd := exec.Command("postgres",
		"-D", pgData,
		"-k", "",
		"-h", "127.0.0.1",
		"-p", fmt.Sprintf("%d", port),
		"-c", "shared_buffers=16MB",
		"-c", "max_connections=20",
		"-c", "dynamic_shared_memory_type=mmap",
		"-c", "fsync=off",
		"-c", "synchronous_commit=off",
		"-c", "full_page_writes=off",
		"-c", "logging_collector=off",
	)
	pgCmd.Stdout = &pgLogs
	pgCmd.Stderr = &pgLogs
	if err := pgCmd.Start(); err != nil {
		t.Fatalf("start postgres: %v", err)
	}

	t.Cleanup(func() {
		if pgCmd.Process != nil {
			_ = pgCmd.Process.Signal(syscall.SIGINT)
			_ = pgCmd.Wait()
		}
	})

	adminDSN := fmt.Sprintf("postgres://postgres@127.0.0.1:%d/postgres?sslmode=disable", port)
	waitForPostgres(t, adminDSN, &pgLogs)

	adminDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(adminDSN)))
	if _, err := adminDB.ExecContext(context.Background(), "CREATE DATABASE eunomia"); err != nil {
		_ = adminDB.Close()
		t.Fatalf("create database eunomia: %v", err)
	}
	_ = adminDB.Close()

	dsn = fmt.Sprintf("postgres://postgres@127.0.0.1:%d/eunomia?sslmode=disable", port)
	database, err := db.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = database.Close()
	})

	return dsn, database
}

// NewMigrated starts an ephemeral PostgreSQL instance and applies all SQL migrations.
func NewMigrated(t *testing.T) (dsn string, database *bun.DB) {
	t.Helper()
	dsn, database = NewRaw(t)
	if _, err := db.Migrate(context.Background(), database); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	return dsn, database
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForPostgres(t *testing.T, dsn string, logs *lockedBuffer) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		err := sqldb.PingContext(ctx)
		cancel()
		_ = sqldb.Close()
		if err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for ephemeral postgres at %s\npostgres logs:\n%s", dsn, logs.String())
}
