# Contributing to hookpost

Thanks for helping! Bug reports, docs fixes and focused pull requests are
all welcome.

## Before you start

- **Bugs:** open an issue with a minimal reproduction (Go version, Postgres
  version, the code that fails, what you expected).
- **Features:** open an issue first and describe the use case. hookpost
  stays small on purpose, so let's agree on the shape before you write code.
- **Security problems:** do not open a public issue; see
  [SECURITY.md](SECURITY.md).

## Development setup

You need Go 1.25+ and Docker (or any PostgreSQL 14+).

```sh
docker run -d --name hookpost-pg -p 5432:5432 \
  -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=hookpost postgres:16
export DATABASE_URL='postgres://postgres:postgres@localhost:5432/hookpost?sslmode=disable'

go test -race ./...
```

Without `DATABASE_URL`, database tests are skipped and only the pure unit
tests run. The test suite uses its own `hookpost_` tables and empties them
when it finishes, so point it at a scratch database.

Also run before pushing:

```sh
gofmt -l .          # must print nothing
go vet ./...
golangci-lint run   # https://golangci-lint.run/welcome/install/
go test -run='^$' -fuzz=FuzzVerify -fuzztime=30s ./signature
```

CI runs all of this on Go 1.25 and the latest Go, against Postgres 14
and 16.

## Rules for pull requests

- **Every bug fix comes with a test** that fails without the fix.
- **No new dependencies in the core module.** The `signature` package must
  stay standard-library only. Integrations (OpenTelemetry, Prometheus, other
  databases) belong in separate sub-modules or behind an interface.
- **Never edit a released migration.** Schema changes go in a new
  `migrations/000N_*.sql` file and must be safe to run on existing data.
- **Keep the public API small.** New exported identifiers need a doc
  comment and, where it helps, an `Example` function.
- **Owner scoping is not optional.** Every query that touches endpoints,
  messages or deliveries on behalf of a caller filters by `owner_id`.
- Add a line under `## [Unreleased]` in [CHANGELOG.md](CHANGELOG.md).
- Keep commits focused; conventional prefixes (`feat:`, `fix:`, `docs:`,
  `test:`, `ci:`, `chore:`) are appreciated.

## Design decisions

The reasoning behind the bigger choices (Postgres only, pgx, BYTEA
payloads, at-least-once delivery, the SSRF guard) is in
[docs/DECISIONS.md](docs/DECISIONS.md). Please read it before proposing a
change to one of them.
