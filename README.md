# hookpost

[![Go Reference](https://pkg.go.dev/badge/github.com/ahmadraza-khan/hookpost.svg)](https://pkg.go.dev/github.com/ahmadraza-khan/hookpost)
[![CI](https://github.com/ahmadraza-khan/hookpost/actions/workflows/ci.yml/badge.svg)](https://github.com/ahmadraza-khan/hookpost/actions)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Reliable outgoing webhooks for Go apps on Postgres: transactional enqueue,
Standard Webhooks signing, retries, dead letters and replay.
No extra service, no extra queue.

## Why

Most SaaS backends end up hand-rolling webhooks: a goroutine that POSTs
JSON. Events get lost when the process crashes, get sent for orders that
were rolled back, go out unsigned, and can't be replayed when a customer's
server was down. hookpost is that code done properly, inside the Postgres
you already run.

## Features

- **Transactional enqueue** (outbox pattern): `Send` writes in your `pgx.Tx`,
  so a webhook exists if and only if your transaction commits
- **[Standard Webhooks](https://www.standardwebhooks.com) signatures**:
  your customers verify in any language with an off-the-shelf library
- **Retries** with backoff and jitter, **dead letters**, manual retry,
  bulk recover and replay
- **Horizontal scaling** via `FOR UPDATE SKIP LOCKED`: run the dispatcher on
  every instance, no leader election
- **SSRF-safe by default**: internal addresses are refused after DNS
  resolution, redirects are never followed
- **Zero-downtime secret rotation**: old and new secret both sign during a
  grace period
- **Drop-in JSON admin API** (`http.Handler`), scoped per customer
- **One dependency** ([pgx](https://github.com/jackc/pgx)); the receiver-side
  `signature` package has none

## Install

```sh
go get github.com/ahmadraza-khan/hookpost
```

Requires Go 1.25+ and PostgreSQL 14+.

## Quick start

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ahmadraza-khan/hookpost"
)

func main() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	wh, _ := hookpost.New(pool, hookpost.Config{})
	if err := wh.Migrate(ctx); err != nil {
		log.Fatal(err)
	}
	go wh.Run(ctx) // background dispatcher; run it on every instance

	ep, _ := wh.CreateEndpoint(ctx, hookpost.EndpointInput{
		OwnerID: "shop_42", URL: "https://example.com/hooks",
	})
	fmt.Println("give this secret to the customer:", ep.Secret)

	tx, _ := pool.Begin(ctx)
	// ... your business writes on tx ...
	_, _ = wh.Send(ctx, tx, hookpost.Message{
		OwnerID: "shop_42", EventType: "order.created",
		Payload: map[string]any{"order_id": 123},
	})
	_ = tx.Commit(ctx) // the webhook is queued only if this commits
	select {}
}
```

`OwnerID` is whoever owns the endpoints (usually your customer or tenant
ID). A message goes to every enabled endpoint of that owner that subscribes
to the event type, or to all event types.

Receivers get a JSON body like this:

```json
{"type": "order.created", "timestamp": "2026-11-02T10:15:00Z", "data": {"order_id": 123}}
```

Set `Config.RawPayload` to send your payload without the envelope.

## Verifying webhooks (receiver side)

In Go, import only the zero-dependency `signature` package:

```go
import "github.com/ahmadraza-khan/hookpost/signature"

http.Handle("/hooks", signature.Middleware(secret, yourHandler))

// or by hand:
err := signature.Verify(secret, r.Header, body)
```

In any other language, use a
[Standard Webhooks library](https://github.com/standard-webhooks/standard-webhooks/tree/main/libraries).
[examples/receiver-node](examples/receiver-node) verifies hookpost
webhooks with the official `standardwebhooks` npm package.

Every request carries the `webhook-id`, `webhook-timestamp` and
`webhook-signature` headers. Verification rejects timestamps more than 5
minutes away from the receiver's clock.

## Retry schedule

Failed attempts (network error, timeout, non-2xx, including 3xx) are retried
with ±10% jitter:

| Attempt | Delay after previous | Elapsed (approx.) |
|--------:|---------------------:|------------------:|
| 1 | immediately | 0 |
| 2 | 5 s | 5 s |
| 3 | 5 min | 5 min |
| 4 | 30 min | 35 min |
| 5 | 2 h | 2.5 h |
| 6 | 5 h | 7.5 h |
| 7 | 10 h | 17.5 h |
| 8 | 10 h | 27.5 h |

After the last attempt the delivery is **dead**. A `410 Gone` response kills
the delivery immediately and disables the endpoint. Set `Config.Schedule` for
your own ladder, e.g. `hookpost.DefaultSchedule[:3]` for 4 attempts.

## Delivery guarantees

- **At-least-once.** If a process dies mid-request, the delivery is sent again
  once its lease expires (request timeout + 30 s). The `webhook-id` header is
  identical on every attempt and replay: **receivers should dedupe on it.**
- **Not ordered.** A retried message can arrive after a newer one.
- **Graceful shutdown.** When the context passed to `Run` is cancelled,
  in-flight requests finish and are recorded before `Run` returns.
- **Disabled endpoints** get no new deliveries; already queued ones wait and
  resume when the endpoint is enabled again.

`Run` uses one pool connection for `LISTEN` (instant wake-up on commit) plus up
to `Config.Workers` for recording results. Behind PgBouncer in transaction
mode, set `Config.DisableListen`; deliveries are then picked up by polling.

## Admin API

Mount the handler behind your own auth. Every route is scoped to the owner
your `Owner` func returns, so one customer can never see another's data:

```go
mux.Handle("/api/webhooks/", http.StripPrefix("/api/webhooks",
	wh.AdminHandler(hookpost.AdminOptions{
		Owner: func(r *http.Request) string { return auth.ShopID(r) },
	})))
```

| Method | Path | |
|---|---|---|
| GET | `/endpoints` | list endpoints |
| POST | `/endpoints` | create (`url`, `description`, `event_types`); the only response with the secret |
| GET | `/endpoints/{id}` | get |
| PATCH | `/endpoints/{id}` | update `url`, `description`, `event_types` |
| DELETE | `/endpoints/{id}` | delete |
| POST | `/endpoints/{id}/enable` | re-enable (e.g. after a 410) |
| POST | `/endpoints/{id}/disable` | disable |
| POST | `/endpoints/{id}/rotate-secret` | `{"grace_seconds": 86400}` → `{"secret": "..."}` |
| POST | `/endpoints/{id}/recover` | re-queue dead deliveries, `?since=<RFC 3339>` |
| GET | `/messages` | `?event_type=&limit=&before=<message id>` |
| GET | `/messages/{id}/deliveries` | status per endpoint |
| POST | `/messages/{id}/resend` | `{"endpoint_id": "..."}` replay to one endpoint |
| GET | `/deliveries/{id}` | get |
| GET | `/deliveries/{id}/attempts` | attempt log with status, error, response body |
| POST | `/deliveries/{id}/retry` | retry now (works on dead deliveries) |

Everything is also available as Go methods (`ListMessages`, `RetryDelivery`,
`RecoverEndpoint`, `ResendMessage`, ...) if you'd rather build your own API.

## Running migrations yourself

`Migrate` is safe to call on every start, even from several instances at
once. If your team manages DDL with goose, atlas or similar, copy
[migrations/*.sql](migrations) into your tool instead and don't call
`Migrate`. The files are plain, idempotent SQL; all tables are prefixed
`hookpost_`.

Finished messages are pruned after `Config.Retention` (30 days by default).

## Try it locally

```sh
docker run -d --name hookpost-pg -p 5432:5432 -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=hookpost postgres:16
export DATABASE_URL='postgres://postgres:postgres@localhost:5432/hookpost?sslmode=disable'

go run ./examples/sender                          # prints a secret, sends an event per second
WEBHOOK_SECRET=whsec_... go run ./examples/receiver
```

Stop the receiver for a while and start it again to watch retries.

## Comparison

| | hookpost | Hosted service (e.g. Svix, Hookdeck) | Self-hosted gateway (e.g. Convoy) | Roll your own |
|---|---|---|---|---|
| Runs as | a library in your app | someone else's SaaS | a separate service | your app |
| Extra infra | none (your Postgres) | none, but a vendor | service + its DB/queue | none |
| Transactional with your writes | **yes** | no (API call) | no (API call) | rarely |
| Cost | free | per message | infra | your time |
| Dashboard UI | no (JSON API) | yes | yes | no |
| Databases | Postgres only | n/a | n/a | any |

The hosted services and gateways are the better choice if you need a
polished customer portal, multi-database support, or don't run Postgres.

## Limitations and roadmap

v0.1 is Postgres-only, uses exact event-type matching (no wildcards) and
stores endpoint secrets in plaintext (see [SECURITY.md](SECURITY.md)).
Planned: wildcard filters, `SendBatch`, encrypted secrets, per-endpoint rate
limits, a `database/sql` adapter, OpenTelemetry, and an embeddable dashboard.
See [docs/ROADMAP.md](docs/ROADMAP.md).

## Contributing

Issues and pull requests are welcome; please read
[CONTRIBUTING.md](CONTRIBUTING.md) first. Security reports go through
[SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)
