# Roadmap

Plans, not promises. Priorities follow what real users ask for: open an
issue if something here (or missing from here) matters to you.

The rule for all of it: **no new dependencies in the core module.**
Integrations ship as sub-modules or behind an interface.

## v0.2: filtering and safety

- Wildcard event filters (`order.*`)
- `SendBatch`: enqueue many messages in one statement
- Encrypted secrets at rest via a pluggable `Config.SecretCipher`
- Per-endpoint rate limiting (token bucket stored in Postgres)
- Auto-disable endpoints after N days of continuous failure
- Custom headers per endpoint

## v0.3: more stores and observability

- Store interface plus a `database/sql` adapter (lib/pq, pgx stdlib)
- MySQL 8 (has `SKIP LOCKED`) and SQLite (single instance)
- `hookpost/otel` sub-module: spans and metrics
- `hookpost/prometheus` sub-module

## v0.4: dashboard

- `hookpost/ui`: an embeddable `http.Handler` with a small UI over the
  admin API (deliveries, attempts, retry buttons)

## v1.0: stable API

- API frozen once 3+ external production users have confirmed it works
  for them
- Documented upgrade and migration policy

## Not planned

- A hosted service or separate gateway process. Embedding is the point.
- Inbound webhook receiving beyond the `signature` package.
