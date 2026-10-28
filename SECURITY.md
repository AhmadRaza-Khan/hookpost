# Security Policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub:
**Security → Report a vulnerability** on
https://github.com/ahmadraza-khan/hookpost (private vulnerability reporting).
Do not open a public issue.

You should get a reply within 72 hours. Fixes are released as a new patch
version and credited in the changelog unless you prefer otherwise.

## Supported versions

Until v1.0, only the latest minor release receives security fixes.

## In scope

- **SSRF protection**: the default HTTP client must refuse loopback,
  private, link-local, CGNAT, multicast and reserved addresses (checked at
  dial time, after DNS resolution) and must not follow redirects.
- **Signature verification** in the `signature` package: anything that lets
  a forged or replayed request pass `Verify`.
- **Owner scoping**: any way for one owner to read or change another
  owner's endpoints, messages or deliveries through the Go API or
  `AdminHandler`.

Out of scope: setups with `Config.AllowPrivateNetworks` or a custom
`Config.HTTPClient`, both of which explicitly turn the SSRF guard off.

## Secrets at rest

Endpoint signing secrets are HMAC keys that hookpost must read back to sign
requests, so they cannot be hashed. In v0.1 they are stored **in plaintext**
in `hookpost_endpoints.secret` (and `old_secret` during rotation).

Protect them like any other credential:

- restrict access to the database and its backups;
- use a dedicated database role for your app and avoid broad read access to
  the `hookpost_` tables;
- rotate a secret (`RotateSecret` or `POST /endpoints/{id}/rotate-secret`)
  if you suspect it leaked.

Encryption at rest through a pluggable cipher is planned for v0.2.
