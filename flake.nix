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
            export EUNOMIA_DATABASE_URL="postgres://postgres@127.0.0.1:54329/eunomia?sslmode=disable"
            export EUNOMIA_TEMPORAL_ADDR="127.0.0.1:7233"
            export EUNOMIA_HTTP_ADDR="127.0.0.1:8080"
            export EUNOMIA_CONFIG_PATH="$EUNOMIA_ROOT/configs/connectors.yaml"

            if [ -f "$EUNOMIA_ROOT/.env" ]; then
              set -a
              # shellcheck disable=SC1091
              source "$EUNOMIA_ROOT/.env"
              set +a
            fi

            echo "🚀 Eunomia dev shell ready ($(go version | awk '{print $3}'), $(postgres --version | awk '{print $1, $3}'), $(temporal --version | head -n1))"
            echo "   Run: process-compose up"
          '';
        };
      });
    };
}
