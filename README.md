# KMS — Key Management Service (Go)

Licence model: one key per contract with a seat quota; endpoints activate with a `device_id` and can never exceed the quota.

Implements the diagram:

| Diagram box        | Implementation                                   |
|--------------------|--------------------------------------------------|
| API GW (kong)      | `kong` container, DB-less, routes in `kong.yml`  |
| Get Key            | `GET /keys/{key}`, `GET /keys?customer_id=`      |
| Purchase           | `POST /purchase`                                 |
| Keygen + Publisher | `internal/keygen`, `internal/queue.Publish`      |
| Aurora             | Postgres 16 (`internal/store`)                   |
| Key Validation     | `POST /validate` → redis → DB lookup on miss     |
| redis              | `internal/cache`                                 |
| SQS (+ Retry)      | Redis Streams consumer group (`internal/queue`)  |
| Subscriber         | `kms -mode subscriber` (`internal/subscriber`)   |

## Web portals (NETRA design language, served by `kms-web` via Kong)

| Path | Purpose | Auth |
|---|---|---|
| `/admin` | Admin console: dashboard per customer, contracts, renew / add seats / re-issue / revoke, activations (admin release), audit with actor, key lookup, new contract, user management, change password | username + password → JWT (sessionStorage only); viewer role sees read-only UI |
| `/portal` | Customer portal: check license status, expiry, seat usage, and whether a given device holds a seat | none (uses public `/validate`) |

Static files live in `web/` (`netra.css` = design tokens, `app.js` = API helper).

## CI/CD

Every push to `main` runs `.github/workflows/build.yml`, which builds `kms`, `kms-kong`, `kms-web` and pushes them to GHCR as `ghcr.io/asdi-r/<name>:latest` and `:sha-<short>`. `coolify-compose.yml` references those images; **Redeploy** in Coolify pulls the new build. Tag `vX.Y.Z` to publish a versioned image.

## API (through Kong, port 8000)

Authentication (handled by kms-api):
- Users: `POST /auth/login` `{"username","password"}` → JWT (8h). Send `Authorization: Bearer <token>`. Roles: `viewer` (read) / `admin` (write + user management). First admin is bootstrapped from `ADMIN_USERNAME`/`ADMIN_PASSWORD` when the users table is empty. Login is rate-limited (5 failures → 15 min lock per IP+username).
- Machines: `X-API-Key: <KMS_API_KEY>` (admin role, actor `apikey`).
- `GET /auth/me`, `POST /auth/password`, `GET|POST /users`, `PATCH /users/{id}` (role/status/password; last active admin is protected).
- Endpoint-facing routes are public: `/validate`, `/activate` (returns a one-time `activation_token`), `/deactivate` (requires that token). Admins release seats with `DELETE /keys/{key}/activations/{device_id}`.
- Every audit event records `actor` (`user:<name>`, `apikey`, `endpoint:<device_id>`).

| Requirement | Endpoint |
|---|---|
| 1. Generate (1/2 yr, N endpoints) | `POST /purchase` `{"customer_id","product","quantity","term_years":1|2}` → one key with `seats` = quantity |
| Activation (seat enforcement) | `POST /activate` `{"key","device_id","hostname?"}` (200 / 409 `seat_limit_reached` / 403 expired/revoked) · `POST /deactivate` `{"key","device_id"}` · `GET /keys/{key}/activations` |
| 2. Re-issuance | `GET /purchases?customer_id=` · `GET /purchases/{id}` (`?include=all` incl. revoked) · `GET /keys?customer_id=` · `GET /keys/{key}` · `POST /keys/{key}/reissue` (new key string, same seats/expiry, activations carried over) |
| 3. Renewal | `POST /purchases/{id}/renew` `{"term_months":N}` (flexible, >= `MIN_RENEW_MONTHS`, no upper bound; `"term_years"` shorthand; optional `"add_quantity"` issues extra keys in the same step) — allowed only within `RENEWAL_WINDOW_DAYS` (60) before expiry or after lapse; extends from current expiry (or from today if lapsed), updates all active keys; otherwise 409 with `renewable_after` |
| 4. Add quantity | `POST /purchases/{id}/keys` `{"quantity"}` — raises the key's seat quota (409 if contract expired) |
| Revoke | `DELETE /keys/{key}` |
| Audit | `GET /purchases/{id}/events` |
| Validate (public) | `POST /validate` `{"key","device_id?"}` → `valid`, `reason` (not_found/expired/revoked/reissued/device_not_activated), `seats`, `used_seats`, `remaining_seats` |

## Env vars

`DATABASE_URL`, `REDIS_URL`, `PORT` (8080), `CACHE_TTL` (10m), `ALLOWED_TERM_YEARS` (1,2),
`DEFAULT_TERM_YEARS` (1), `RENEWAL_WINDOW_DAYS` (60), `MIN_RENEW_MONTHS` (1), `MAX_QUANTITY` (1000),
`QUEUE_STREAM` (kms.keys), `QUEUE_DLQ` (kms.keys.dlq), `MAX_RETRIES` (5), `RETRY_DELAY` (5s),
`JWT_SECRET` (required, ≥32 chars), `JWT_TTL` (8h), `KMS_API_KEY` (optional machine key), `ADMIN_USERNAME` (admin), `ADMIN_PASSWORD` (bootstrap), `LOGIN_MAX_FAILS` (5), `LOGIN_LOCKOUT` (15m), `WEBHOOK_URL` (optional; subscriber POSTs each event there).

Queue event types: `key.issued`, `key.reissued`, `key.renewed`, `key.seats_added`, `key.activated`, `key.deactivated`.

## Retry semantics

Subscriber errors leave the message pending; it is re-claimed after `RETRY_DELAY`
(`XAUTOCLAIM`). After `MAX_RETRIES` the message is copied to the DLQ stream and acked.
Delivery is recorded idempotently in `deliveries` (unique `message_id`).

## Local dev

```
docker compose up --build
```

## Deploy (Coolify on 116.212.74.168)

Images are built on the server from `/opt/kms`:

```
docker build -t kms:latest .
docker build -t kms-kong:latest -f Dockerfile.kong .
```

Then in Coolify: New Resource → Docker Compose Empty → paste `coolify-compose.yml`
→ set domain on `kong` → Deploy. Redeploy after rebuilding images.

To swap Redis Streams for real AWS SQS, replace `internal/queue` (same `Publish`/`Consume` interface).
