# Project Eunomia

Single-tenant internal **Generic Agentic Workflow** engine built on **Deterministic Rails + Bounded Agency** (`Go` · `ConnectRPC` · `Temporal` · `Google ADK Go` · `PostgreSQL / bun` · `Nix`).

## Local Dev Quickstart (Zero-Docker)

### 1. Install Nix (once per machine)

Recommended on macOS/Linux ([Determinate Nix Installer](https://github.com/DeterminateSystems/nix-installer) — enables Flakes out of the box and supports clean uninstall):

```bash
curl --proto '=https' --tlsv1.2 -sSf -L https://install.determinate.systems/nix | sh -s -- install
```

Open a new terminal tab after installation completes.

### 2. Enter the Nix dev shell & boot the stack

```bash
nix develop
process-compose up
```

`nix develop` automatically generates a local gitignored master key at `.dev/master.key` if `EUNOMIA_MASTER_KEYS` is not set in `.env`. In production, **`EUNOMIA_DATABASE_URL`** and **`EUNOMIA_MASTER_KEYS`** are strictly required (see `.env.example`).

`process-compose up` boots the full local stack natively:

| Service | Address | Notes |
| --- | --- | --- |
| **PostgreSQL** | `127.0.0.1:54329` | Ephemeral DB in `.dev/pgdata` (`eunomia` DB auto-created; non-default port avoids host PG conflicts) |
| **Migrations** | one-shot | Runs `go run ./cmd/eunomia migrate` before `serve` starts |
| **Temporal Dev Server** | `127.0.0.1:7233` | Persisted to `.dev/temporal.db` |
| **Temporal Web UI** | http://127.0.0.1:8233 | Inspect workflows, signals, and SLA timers |
| **Eunomia ConnectRPC** | `127.0.0.1:8080` | Pure gRPC / ConnectRPC (h2c); health via `grpc.health.v1.Health` + reflection |

Verify health & reflection from another terminal inside `nix develop`:

```bash
grpc-health-probe -addr=127.0.0.1:8080
grpcurl -plaintext 127.0.0.1:8080 list
```

Press `Ctrl+C` (or `F10` in the TUI) for clean teardown. Wipe local state anytime with `rm -rf .dev`.

## Managing Connectors & Secrets

Zero credentials live in YAML. Connector settings/aliases live in `tenant_connectors` and envelope-encrypted credentials live in `tenant_secrets`:

```bash
# Generate a new 32-byte master key for EUNOMIA_MASTER_KEYS
go run ./cmd/eunomia secret keygen

# Store a secret from stdin or a file (never passed as CLI arg)
printf '%s' "$MY_NOTION_TOKEN" | go run ./cmd/eunomia secret set --connector notion --key token
go run ./cmd/eunomia secret set --connector google_workspace --key service_account_json --file ./sa.json

# List stored secret metadata (never prints values)
go run ./cmd/eunomia secret list

# Rotate master key (after prepending v2:<b64> to EUNOMIA_MASTER_KEYS)
go run ./cmd/eunomia secret rotate-master-key

# Configure non-secret connector settings & resource aliases
go run ./cmd/eunomia connector set --type notion --enabled=true --resource onboarding_tracker=<db-id>
go run ./cmd/eunomia connector set --type google_workspace --enabled=true --setting domain=spacelions.com --setting impersonate_email=ops@spacelions.com
go run ./cmd/eunomia connector list
```

## Running Tests

```bash
go test ./...
```
