{
  description = "Project Eunomia — Generic Agentic Workflow Engine (Zero-Docker Dev Stack)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      supportedSystems = [
        "aarch64-darwin"
        "x86_64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forEachSystem = f:
        nixpkgs.lib.genAttrs supportedSystems (system:
          f {
            pkgs = import nixpkgs { inherit system; };
          });
    in
    {
      devShells = forEachSystem ({ pkgs }: {
        default = pkgs.mkShell {
          name = "eunomia-dev";

          packages = with pkgs; [
            go
            gopls
            golangci-lint
            postgresql
            temporal-cli
            process-compose
            buf
            grpcurl
            grpc-health-probe
            lefthook
          ];

          shellHook = ''
            # Isolate Nix Go toolchain from host goenv/asdf/Homebrew GOROOT
            unset GOROOT
            export GOTOOLCHAIN="local"

            export EUNOMIA_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
            export PGHOST="127.0.0.1"
            export PGPORT="54329"
            export PGUSER="postgres"
            export PGDATABASE="eunomia"
            export PGDATA="$EUNOMIA_ROOT/.dev/pgdata"
            export PC_PORT_NUM="8089"

            export EUNOMIA_GRPC_ADDR="127.0.0.1:8080"
            export EUNOMIA_DATABASE_URL="postgres://postgres@127.0.0.1:54329/eunomia?sslmode=disable"
            export EUNOMIA_TEMPORAL_ADDR="127.0.0.1:7233"
            export EUNOMIA_TEMPORAL_NAMESPACE="default"
            export EUNOMIA_TEMPORAL_TASK_QUEUE="eunomia"
            export EUNOMIA_DEFAULT_TENANT="spacelions"

            if [ -f "$EUNOMIA_ROOT/.env" ]; then
              set -a
              # shellcheck disable=SC1091
              source "$EUNOMIA_ROOT/.env"
              set +a
            fi

            # Zero-touch local dev master key (gitignored in .dev/master.key).
            # Production MUST set EUNOMIA_MASTER_KEYS explicitly; Go fails fast if unset.
            if [ -z "''${EUNOMIA_MASTER_KEYS:-}" ]; then
              mkdir -p "$EUNOMIA_ROOT/.dev"
              if [ ! -f "$EUNOMIA_ROOT/.dev/master.key" ]; then
                umask 077
                printf 'v1:%s\n' "$(head -c 32 /dev/urandom | base64 | tr -d '\n')" > "$EUNOMIA_ROOT/.dev/master.key"
                echo "🔑 Generated local dev master key at .dev/master.key (gitignored)"
              fi
              export EUNOMIA_MASTER_KEYS="$(cat "$EUNOMIA_ROOT/.dev/master.key")"
            fi

            if [ -d "$EUNOMIA_ROOT/.git" ] && [ -f "$EUNOMIA_ROOT/lefthook.yml" ]; then
              if ! grep -q "lefthook" "$EUNOMIA_ROOT/.git/hooks/pre-commit" 2>/dev/null; then
                (cd "$EUNOMIA_ROOT" && lefthook install >/dev/null 2>&1) || true
              fi
            fi

            echo "🚀 Eunomia dev shell ready ($(go version | awk '{print $3}'), $(postgres --version | awk '{print $1, $3}'), $(temporal --version | head -n1))"
            echo "   Run: process-compose up"
          '';
        };
      });
    };
}
