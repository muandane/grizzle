{ pkgs, lib, config, inputs, ... }:

{
  # Environment Variables
  env = {
    DATABASE_URL = "postgres://127.0.0.1:5432/grizzle_test?sslmode=disable";
  };

  # Go Language Support
  languages.go = {
    enable = true;
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

  # Developer Utility Scripts (available directly in the devenv shell)
  scripts = {
    "test-all".exec = ''
      echo "Running all unit and integration tests..."
      go test -v -race ./...
    '';

    "lint".exec = ''
      echo "Running golangci-lint..."
      golangci-lint run ./...
    '';

    "db-shell".exec = ''
      psql "$DATABASE_URL"
    '';

    "db-reset".exec = ''
      echo "Resetting test database..."
      psql "$DATABASE_URL" -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"
      echo "Database schema reset successfully."
    '';
  };

  # Enter shell greeting & checks
  enterShell = ''
    echo "🐻 Grizzle Development Environment (Nix + devenv)"
    echo "• Go version:       $(go version)"
    echo "• PostgreSQL tools: $(psql --version)"
    echo "• Database URL:     $DATABASE_URL"
    echo ""
    echo "Available commands:"
    echo "  devenv up   -> Start background PostgreSQL daemon"
    echo "  db-shell    -> Connect to local test database via psql"
    echo "  db-reset    -> Wipe and recreate public schema"
    echo "  test-all    -> Run Go tests with race detector"
    echo "  lint        -> Run golangci-lint"
  '';
}
