# Design decisions

Short records of choices that shape the API or the schema, and why.

## Postgres only (v0.1)

`FOR UPDATE SKIP LOCKED` plus transactions make leasing correct without a
separate queue, and most Go SaaS backends already run Postgres. Other
databases can come later behind a store interface.

## pgx v5, not database/sql

`Send` must join the caller's transaction, and pgx is what most Postgres Go
apps use. A `database/sql` adapter is planned.

## Standard Webhooks for signing

Receivers can verify with an existing library in any language instead of
custom code. Verified against the spec's test vector and the
`standardwebhooks` npm package.

## Payload stored as BYTEA, not JSONB

JSONB normalises key order and whitespace. Storing exact bytes means the
signed body is byte-identical on every attempt and replay. The envelope is
built before storing, so the stored bytes are the request body.

## Send is one SQL statement

The message and its deliveries are inserted by a single statement with
data-modifying CTEs, so `Send` is atomic even when called with the pool
rather than a transaction.

## At-least-once with a stale-worker guard

Leasing bumps `attempts` and pushes `next_attempt_at` past the request
timeout; no transaction is held during HTTP. Result updates only apply
when `attempts` still matches the lease, so a worker whose lease expired
can't overwrite a newer outcome. The outcome and its attempt row are
written in one transaction.

## Manual retry keeps the attempt counter

`RetryDelivery` on a dead delivery gives exactly one more attempt.
`RecoverEndpoint` and `ResendMessage` reset the counter for a full new
schedule.

## SSRF guard at dial time

Checking the URL's hostname is not enough (DNS can point at, or rebind
to, an internal address). The dialer checks the resolved IP, and redirects
are never followed.

## Go 1.25 minimum

Set by the dependencies: pgx v5.9.2+ (fixes GO-2026-5004, SQL injection via
dollar-quoted placeholders) and golang.org/x/text v0.39+ (GO-2026-5970,
GO-2026-6629) require Go 1.25. The first plan was Go 1.22 with pgx v5.7.4,
but govulncheck showed hookpost reaches those vulnerable code paths, and a
webhook library should not ship with a known SQL injection in its driver.
Go 1.25 also covers what the code itself needs (ServeMux patterns,
`math/rand/v2`). CI tests the minimum and the latest stable Go.

Tests use `DATABASE_URL` rather than testcontainers-go to keep the test
dependency tree small; CI provides Postgres as a service container.

## MIT license
