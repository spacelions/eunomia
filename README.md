# Project Eunomia

Single-tenant internal **Generic Agentic Workflow** engine built on **Deterministic Rails + Bounded Agency** (`Go` · `Temporal` · `Google ADK Go` · `PostgreSQL / bun` · `Nix`).

## Local Dev Quickstart (Zero-Docker)

### 1. Install Nix (once per machine)

Recommended on macOS/Linux ([Determinate Nix Installer](https://github.com/DeterminateSystems/nix-installer) — enables Flakes out of the box and supports clean uninstall):

```bash
curl --proto '=https' --tlsv1.2 -sSf -L https://install.determinate.systems/nix | sh -s -- install
```

Open a new terminal tab after installation completes.

### 2. Configure local environment

```bash
cp .env.example .env
cp configs/connectors.example.yaml configs/connectors.yaml
```

### 3. Enter the Nix dev shell & boot the stack

```bash
nix develop
process-compose up
```

This boots the full local stack natively:

| Service | Address | Notes |
| --- | --- | --- |
| **PostgreSQL** | `127.0.0.1:54329` | Ephemeral DB in `.dev/pgdata` (`eunomia` DB auto-created; non-default port avoids host PG conflicts) |
| **Temporal Dev Server** | `127.0.0.1:7233` | Persisted to `.dev/temporal.db` |
| **Temporal Web UI** | http://127.0.0.1:8233 | Inspect workflows, signals, and SLA timers |
| **Eunomia Server** | http://127.0.0.1:8080 | Health check at `/healthz` |

Press `Ctrl+C` (or `F10` in the TUI) for clean teardown. Wipe local state anytime with `rm -rf .dev`.

## Running Tests

```bash
go test ./...
```
