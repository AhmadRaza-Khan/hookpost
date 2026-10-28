# Changelog

All notable changes to this project are documented here. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project follows [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-10-28

First release. Requires Go 1.25+ and PostgreSQL 14+.

### Added

- `signature` package: Standard Webhooks signing and verification,
  `Middleware` for receivers. No dependencies outside the standard library.
- Postgres schema with embedded, concurrency-safe `Migrate`.
- `Send`: transactional enqueue in a `pgx.Tx`, idempotent message IDs,
  fan-out to matching endpoints, `{"type","timestamp","data"}` envelope
  (or `Config.RawPayload`).
- Dispatcher (`Run`): `SKIP LOCKED` leasing, safe on multiple instances,
  exponential backoff with jitter, dead letters, `410 Gone` disables the
  endpoint, LISTEN/NOTIFY wake-ups, graceful shutdown, retention pruning.
- Endpoints: owner-scoped CRUD, event-type filters, zero-downtime secret
  rotation.
- SSRF-safe HTTP client (blocks internal addresses after DNS resolution,
  never follows redirects).
- `AdminHandler`: owner-scoped JSON API for endpoints, messages,
  deliveries, attempts, retry, recover and resend.

[Unreleased]: https://github.com/ahmadraza-khan/hookpost/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/ahmadraza-khan/hookpost/releases/tag/v0.1.0
