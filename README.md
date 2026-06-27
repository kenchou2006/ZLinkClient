# ZLinkClient

The fast redirect engine for the ZLink URL shortener.

**ZLinkClient** is a small, dependency-light Go service whose only job is to
resolve short codes and redirect end users as quickly as possible. It reads
from the same PostgreSQL database and Redis cache as the management API, so
links created through the admin UI or API are served here instantly.

> Link/user **management** lives in the Django API
> ([ZLinkAPI](https://github.com/kenchou2006/ZLinkAPI)); the admin interface
> is [ZLinkFE](https://github.com/kenchou2006/ZLinkFE). This service is intentionally
> read-only and stateless.

## How it works

1. A request comes in for `GET /{short_code}` (or `/`, which maps to the special code `@root`).
2. Look up the Redis cache (key format `:{CACHE_VERSION}:shortener:url:{code}`).
3. **Hit** → `302` redirect to the original URL, and the TTL is refreshed (unless persistent).
4. **Miss** → query the `shortener_link` table, backfill the cache, then redirect.
5. **Not found or expired** → render the built-in 404 page. Links past their
   `expires_at` never redirect, and an expired entry found in the cache is
   dropped on access.
6. On every hit/miss a GA4 event is reported asynchronously (when GA is configured).

> The table/column names (`shortener_link` / `short_code` / `original_url`) and
> the cache key prefix are fixed in code (see `server/router.go`) to stay in
> sync with the Django side — don't change them in isolation.

## Why a separate service?

Redirects are the hot path: they must be cheap, fast, and horizontally
scalable. Keeping them in a compiled Go binary (rather than the Django app)
means the redirect path stays snappy and can be deployed independently —
including as a serverless function.

## Tech stack

Go 1.24 · `lib/pq` (PostgreSQL) · `go-redis` · `godotenv`

## Requirements

- Go 1.24+
- PostgreSQL (the same database used by the ZLink API)
- Redis (optional but recommended — easy via Docker, see below)

## Quick start

```bash
cd ZLinkClient

go mod download
cp .env.template .env                 # then edit it

go run .                              # listens on :8080 by default
```

Test it:

```bash
curl http://localhost:8080/healthz            # {"status":"ok"}
curl -i http://localhost:8080/<short_code>     # 302 redirect
```

### Run Redis with Docker

```bash
docker run -d --name zlink-redis -p 6379:6379 redis:7-alpine
```

Set `REDIS_URL=redis://localhost:6379`. If developing alongside the ZLink API
on the same machine, both can share this one Redis container. Redis is optional
— without it the service falls back to querying the database directly.

## Configuration

| Variable | Description |
|----------|-------------|
| `POSTGRES_URL` | PostgreSQL connection string (also accepts `DATABASE_URL`) |
| `REDIS_URL` | Redis connection, e.g. `redis://localhost:6379` (also accepts `REDIS_ADDR`) |
| `CACHE_TTL` | Cache seconds; `None`/unset = persistent |
| `CACHE_VERSION` | Cache key version prefix, default `0` (must match the Django side) |
| `GA_MEASUREMENT_ID` / `GA_API_SECRET` | GA4 config; click tracking is disabled when unset |
| `GA4_TIMEOUT` / `GA4_ASYNC` | GA4 timeout seconds / async reporting (default 3 / true) |
| `PORT` | Listen port, default `8080` |

## Routes

| Path | Description |
|------|-------------|
| `GET /healthz` | Health check |
| `GET /static/*` | Static files (if a `static/` directory exists) |
| `GET /` | Resolves the special `@root` code |
| `GET /{short_code}` | Resolves and `302`-redirects; `404` if unknown |

## Deployment

`vercel.json` deploys it as a serverless function (entry point `Handler` in
`api/main.go`), routing all paths to it. For production set `POSTGRES_URL`,
`REDIS_URL`, optional GA4 variables, and a `CACHE_VERSION` matching the Django
side.

## License

MIT — see [LICENSE](LICENSE).
