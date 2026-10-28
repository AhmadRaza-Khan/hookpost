// Package hookpost sends reliable, signed outgoing webhooks from Go
// applications backed by Postgres: transactional enqueue, Standard Webhooks
// signing, retries, dead letters and replay. No extra service, no extra queue.
//
// # Overview
//
// Create one Client per process, run its dispatcher in the background, and
// call Send inside the same transaction as your business writes:
//
//	wh, _ := hookpost.New(pool, hookpost.Config{})
//	_ = wh.Migrate(ctx)
//	go wh.Run(ctx)
//
//	tx, _ := pool.Begin(ctx)
//	// ... create the order on tx ...
//	_, _ = wh.Send(ctx, tx, hookpost.Message{
//		OwnerID: shopID, EventType: "order.created", Payload: order,
//	})
//	_ = tx.Commit(ctx)
//
// Customers register endpoints (CreateEndpoint, or the JSON API from
// AdminHandler) and verify what they receive with the zero-dependency
// signature package, or any Standard Webhooks library in any language.
//
// # Guarantees
//
//   - Transactional: Send only writes rows. If the transaction rolls back,
//     nothing is sent; once it commits, the webhook survives crashes and
//     restarts until it is delivered or dead.
//   - At-least-once: a delivery may arrive more than once (for example when
//     a process dies mid-request). The webhook-id header is the same on
//     every attempt and replay; receivers should dedupe on it.
//   - Horizontally scalable: Run on every instance. Deliveries are leased
//     with FOR UPDATE SKIP LOCKED, so instances never send the same
//     attempt concurrently and never block each other.
//   - Retries: failed attempts are retried on Config.Schedule (8 attempts
//     over about 27 hours by default), then marked dead. Dead deliveries can
//     be retried, recovered or resent.
//   - SSRF-safe: by default the HTTP client refuses loopback, private,
//     link-local and other internal addresses, checked after DNS
//     resolution, and never follows redirects.
//
// # Ordering
//
// Deliveries are not ordered. A retried message can arrive after a newer
// one; include a timestamp or version in your payload if order matters.
//
// # Migrations
//
// Migrate creates hookpost's tables (all prefixed "hookpost_"). Teams that
// manage DDL themselves can copy migrations/*.sql into their own tool
// instead and never call Migrate.
package hookpost
