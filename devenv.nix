{ pkgs, lib, config, inputs, ... }:

{
  # Environment Variables
  env = {
    DATABASE_URL = "postgres://127.0.0.1:5432/grizzle_test?sslmode=disable";
    POSTGRES_DSN = "postgres://127.0.0.1:5432/grizzle_test?sslmode=disable";
    # Stay on the devenv/local Go toolchain. "auto" can pull a mismatched
    # toolchain that disagrees with languages.go's GOROOT.
    GOTOOLCHAIN = lib.mkForce "local";
  };

  # Go Language Support
  languages.go = {
    enable = true;
    # Pin to the same minor as go.mod so GOROOT matches the `go` on PATH.
    # Without this, nixpkgs' default Go can lag go.mod and golangci-lint
    # typecheck fails with "version does not match go tool version".
    package = pkgs.go_1_27;
  };

  # Packages needed for development, linting, and database interaction
  packages = [
    pkgs.git
    pkgs.golangci-lint
    pkgs.gotools
    pkgs.postgresql_16 # Provides psql, pg_dump, etc.
  ];

  # Automated PostgreSQL Service for local dev & integration tests
  # Start with: `devenv up`
  services.postgres = {
    enable = true;
    package = pkgs.postgresql_16;
    listen_addresses = "127.0.0.1";
    port = 5432;
    initialDatabases = [
      { name = "grizzle_test"; }
    ];
  };

  # Local git hooks (pre-commit & pre-push) that mirror CI quality gates.
  # Hooks are installed into .git/hooks when entering the devenv shell, so
  # contributions are validated locally BEFORE they ever reach remote /
  # GitHub Actions. A generated .pre-commit-config.yaml stays gitignored.
  git-hooks.hooks = {
    # pre-commit: fail fast on unformatted Go files
    gofmt-check = {
      enable = true;
      always_run = true;
      name = "gofmt check";
      description = "Verify all Go files are gofmt-formatted";
      entry = builtins.toString (pkgs.writeShellScript "gofmt-check" ''
        out=$(git ls-files '*.go' | xargs gofmt -l)
        if [ -n "$out" ]; then
          echo "Files not gofmt-formatted:"
          echo "$out"
          echo "Run 'fmt' and re-stage."
          exit 1
        fi
      '');
      pass_filenames = false;
      stages = [ "pre-commit" ];
    };

    # pre-commit: static analysis on both modules
    go-vet = {
      enable = true;
      always_run = true;
      name = "go vet";
      description = "go vet across root and otelgrizzle modules";
      entry = builtins.toString (pkgs.writeShellScript "go-vet" ''
        go vet ./... || exit 1
        (cd otelgrizzle && go vet ./...) || exit 1
      '');
      pass_filenames = false;
      stages = [ "pre-commit" ];
    };

    # pre-commit: golangci-lint on both modules (mirrors CI lint job)
    go-lint = {
      enable = true;
      always_run = true;
      name = "golangci-lint";
      description = "golangci-lint across root and otelgrizzle modules";
      entry = builtins.toString (pkgs.writeShellScript "go-lint" ''
        golangci-lint run ./... || exit 1
        (cd otelgrizzle && golangci-lint run ./...) || exit 1
      '');
      pass_filenames = false;
      stages = [ "pre-commit" ];
    };

    # pre-push: full unit test suite with race detector (mirrors CI unit job)
    go-unit-tests = {
      enable = true;
      always_run = true;
      name = "go unit tests";
      description = "Race-enabled unit tests before any push to remote";
      entry = builtins.toString (pkgs.writeShellScript "go-unit-tests" ''
        go test -race -count=1 ./... || exit 1
        (cd otelgrizzle && go test -race -count=1 ./...) || exit 1
      '');
      pass_filenames = false;
      stages = [ "pre-push" ];
    };
  };

  # Developer Utility Scripts (available directly in the devenv shell)
  scripts = {
    "fmt".exec = ''
      go fmt ./...
      (cd otelgrizzle && go fmt ./...)
    '';

    "vet".exec = ''
      go vet ./...
      (cd otelgrizzle && go vet ./...)
    '';

    "lint".exec = ''
      echo "Running golangci-lint..."
      golangci-lint run ./...
      (cd otelgrizzle && golangci-lint run ./...)
    '';

    "test".exec = ''
      go test -race -count=1 ./...
      (cd otelgrizzle && go test -race -count=1 ./...)
    '';

    "test-integration".exec = ''
      echo "Running integration tests..."
      # -p 1 matches CI: packages share one Postgres and cluster-global
      # roles/slots must not race across packages.
      set -o pipefail
      go test -tags integration -race -count=1 -p 1 -v ./... | tee /tmp/grizzle-integration.log
      pass_count=$(grep -c "^--- PASS" /tmp/grizzle-integration.log || true)
      fail_count=$(grep -c "^--- FAIL" /tmp/grizzle-integration.log || true)
      echo "Executed passing integration tests: $pass_count"
      if [ "$pass_count" -eq 0 ]; then
        echo "Error: No integration tests were executed! Run 'devenv up' first to start PostgreSQL."
        exit 1
      fi
      if [ "$fail_count" -ne 0 ]; then
        echo "Error: $fail_count integration test(s) failed."
        exit 1
      fi
    '';

    "golden-update".exec = ''
      go test -run TestPlan_GoldenFile -update .
    '';

    "ci".exec = ''
      lint && vet && test && test-integration
    '';

    "db-shell".exec = ''
      psql "$DATABASE_URL"
    '';

    "db-reset".exec = ''
      echo "Resetting test database..."
      psql "$DATABASE_URL" -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"
      echo "Database schema reset successfully."
    '';

    "clean".exec = ''
      echo "Cleaning generated SQLite databases, test logs, and build artifacts..."
      find . -type f \( -name "*.db" -o -name "*.db-*" -o -name "*.sqlite" -o -name "*.sqlite-*" -o -name "*.out" -o -name "coverage.txt" -o -name "*.prof" -o -name "*.log" \) -delete
      echo "Clean complete."
    '';
  };

  # Enter shell greeting & checks
  enterShell = ''
    # Prefer the languages.go toolchain over any host Go that may shadow it
    # on PATH (e.g. Homebrew). A mismatched GOROOT/binary pair breaks
    # golangci-lint typecheck with "version does not match go tool version".
    if [ -n "''${GOROOT:-}" ] && [ -x "$GOROOT/bin/go" ]; then
      export PATH="$GOROOT/bin:$PATH"
    else
      unset GOROOT
      export GOROOT="$(go env GOROOT)"
      export PATH="$GOROOT/bin:$PATH"
    fi

    echo "🐻 Grizzle Development Environment (Nix + devenv)"
    echo "• Go version:       $(go version)"
    echo "• GOROOT:           $GOROOT"
    echo "• PostgreSQL tools: $(psql --version)"
    echo "• Database URL:     $DATABASE_URL"
    echo ""
    echo "Git hooks installed (run locally before push):"
    echo "  pre-commit -> gofmt check, go vet, golangci-lint"
    echo "  pre-push   -> go test -race -count=1 (root + otelgrizzle)"
    echo ""
    echo "Available commands:"
    echo "  devenv up       -> Start background PostgreSQL daemon"
    echo "  fmt             -> go fmt both modules"
    echo "  vet             -> go vet both modules"
    echo "  lint            -> golangci-lint both modules"
    echo "  test            -> Unit tests with race detector (both modules)"
    echo "  test-integration-> Integration tests against PostgreSQL (needs devenv up)"
    echo "  golden-update   -> Regenerate golden plan/export fixtures"
    echo "  ci              -> lint + vet + test + test-integration (full local gate)"
    echo "  db-shell        -> Connect to local test database via psql"
    echo "  db-reset        -> Wipe and recreate public schema"
    echo "  clean           -> Remove generated SQLite files and test artifacts"
  '';
}