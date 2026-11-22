# AGENTS.md — Project Eunomia

Guidelines for AI coding agents and contributors working in `github.com/spacelions/eunomia`.

## 1. Environment & Local Dev Stack

- **Zero-Docker Nix stack:** All toolchain dependencies (`go`, `postgresql`, `temporal-cli`, `process-compose`, `buf`, `grpcurl`, `grpc-health-probe`, `lefthook`, `golangci-lint`) are pinned in `flake.nix` / `flake.lock`.
- Always run Go, PostgreSQL, Temporal, and `buf` commands inside `nix develop` (the `shellHook` unsets host `GOROOT` and sets `GOTOOLCHAIN=local` to avoid conflicts with host `goenv`/Homebrew).
- Start the full local stack with `process-compose up` (PostgreSQL on `127.0.0.1:54329`, Temporal on `7233` / Web UI `8233`, Eunomia ConnectRPC on `8080`).

## 2. Architecture Conventions

- **API surface:** `cmd/eunomia` is pure **ConnectRPC (gRPC over h2c)** using `connectrpc.com/*` and `buf` for Protobuf codegen. Do not add hand-rolled REST/HTTP endpoints to the core engine.
- **CLI & Config:** Use **Cobra** (`github.com/spf13/cobra`) and **Viper** (`github.com/spf13/viper`) with isolated `viper.New()` instances (no global Viper state).
- **Secrets & Connectors (no credentials in YAML):**
  - Never put credentials in YAML files or pass secrets as CLI arguments.
  - Non-secret per-tenant connector settings and resource aliases live in Postgres `tenant_connectors` (`internal/connectorstore`).
  - Credentials live in Postgres `tenant_secrets` via `internal/secrets.DBSecretProvider` using AES-256-GCM envelope encryption (AAD bound to `tenant:connector:key`) behind the `KeyWrapper` interface (`LocalKeyWrapper` in v0.1, `AWSKMSKeyWrapper` in prod).
  - Keep `tenant_id` in all connector/secret interfaces and tables even while v0.1 runs single-tenant.
- **Database Migrations (`bun/migrate`):**
  - Migrations live in `internal/db/migrations/` as embedded `.up.sql` / `.down.sql` pairs.
  - Every migration **must** use a 14-digit `YYYYMMDDHHMMSS` timestamp prefix (never sequential `001_`, `002_`) — enforced by `TestMigrationsUseTimestampPrefix`.
  - Migrations run **only** via explicit `eunomia migrate` — `eunomia serve` must never auto-migrate.
- **Temporal Determinism:** Google ADK Go and any non-deterministic I/O must run only inside Temporal Activities or the Build-Time compiler — never inside Temporal workflow code.

## 3. Testing & `test/` Directory Rules

- **`test/` directory for shared test code:**
  - All shared test helpers, ephemeral DB/service harnesses, test fixtures, mocks, and cross-package E2E suites **must** live under the top-level `test/` directory (e.g. `test/testpg`, `test/fixtures`, `test/e2e`).
  - Never create test-only helper packages under `internal/`.
  - Standard Go unit tests (`*_test.go`) remain colocated with the package they test.
- **Assertions & Diffs:**
  - Use **`github.com/stretchr/testify/require`** (fatal assertions) and **`github.com/stretchr/testify/assert`** (non-fatal assertions).
  - Use **`github.com/google/go-cmp/cmp`** (`cmp.Diff`) for struct, slice, and map comparisons.
- **Real PostgreSQL in tests:** Use `test/testpg` (`testpg.NewRaw(t)` / `testpg.NewMigrated(t)`) to spin up fast ephemeral Postgres instances on random local TCP ports — do not mock SQL queries when real Postgres tests run in ~1–2s.
- **Coverage Gate (>= 95.0%):**
  - `lefthook` runs `gofmt`, `go vet ./...`, and `scripts/check-coverage.sh` (`go test -race` + total statement coverage >= `95.0%`, excluding `test/` helpers and `cmd/eunomia/main.go`) on every `git commit`.
  - Never bypass hooks with `--no-verify`.

## 4. Git & PR Workflow

- `main` is branch-protected. Always create a feature branch and open a pull request.
- Keep Notion task status ([Project Eunomia](https://app.notion.com/p/4201c40cbff1818e980ae68d8c316a74)) aligned with PR state (`In Progress` → `In Review` → `Done`).
