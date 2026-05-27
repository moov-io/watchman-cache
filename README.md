# watchman-cache

A production-hardened nginx reverse-proxy cache for [moov/watchman](https://github.com/moov-io/watchman) sanctions list downloads (v0.62.0+).

Government list endpoints (OFAC, trade.gov/Azure, S3 pre-signed URLs, EU, etc.) are frequently flaky. Large CSV responses are regularly truncated mid-transfer, IPv6 records cause connection failures, and 302 redirects to short-lived S3 URLs cause watchman to bypass any cache and hit the origins directly. The result is repeated "unexpected EOF", "max retries", and fatal "problem during initial download" crashes during startup.

This project fronts watchman with a tightly scoped nginx cache that:

- Speaks the exact URL template mechanism watchman uses (`OFAC_DOWNLOAD_TEMPLATE`, `US_CSL_DOWNLOAD_TEMPLATE`, `US_NON_SDN_DOWNLOAD_TEMPLATE`, etc.)
- Intercepts 302 redirects internally so watchman never sees them
- Applies aggressive buffering, long timeouts, retries, and SNI/Host fixes on the known-problematic paths
- Persists complete responses in a named Docker volume so subsequent restarts are fast cache hits

## Features

- **Exact allow-list** — only the files watchman actually requests are served (no open proxy)
- **302 redirect following** inside nginx (`@follow_redirect`) for all OFAC/Non-SDN files so the final CSV is cached under the stable filename
- **Hardened large-file handling** — 512 KiB buffers, 180 s read timeouts, `proxy_next_upstream` retries on the critical paths
- **IPv6 safety** — resolver configured with `ipv6=off` (public DNS with fallback behavior)
- **Long-lived cache** — 48 h for the large/flaky lists, 7 d inactive eviction on the cache zone
- **Stale-while-revalidate** + background updates for resilience during origin outages
- **Named volume persistence** — the single most important production lever
- Pure-stdlib Go integration test that validates the full cold-start flow

## Quick Start

```bash
# 1. Start everything (builds the cache image + pulls watchman:v0.62.0)
make up

# 2. Wait for watchman to finish loading lists (30–120 s on a cold cache)
make ping
# → PONG

# 3. Watch cache behavior (MISS → later HITs)
make logs-cache

# 4. Tear everything down (including the cache volume)
make down
```

The cache listens on **localhost:3000** (easy to match common `http://localhost:3000/%s` examples). Watchman is on the usual 8084/9094 ports.

## Wiring Watchman to the Cache

Point watchman at the cache using its normal environment variables:

```yaml
services:
  watchman:
    image: moov/watchman:v0.62.0
    environment:
      - INCLUDED_LISTS=us_ofac,us_non_sdn,us_csl,us_fincen_311,eu_csl

      # Templated lists (most common)
      - OFAC_DOWNLOAD_TEMPLATE=http://cache:8080/%s
      - US_CSL_DOWNLOAD_TEMPLATE=http://cache:8080/%s
      - US_NON_SDN_DOWNLOAD_TEMPLATE=http://cache:8080/%s

      # Non-templated lists
      - EU_CSL_DOWNLOAD_URL=http://cache:8080/eu_csl.csv
      - FINCEN_311_DOWNLOAD_URL=http://cache:8080/fincen_311.html
```

### Supported Lists

| List            | Watchman Env Var(s)                     | nginx path on cache          | Notes                              |
|-----------------|-----------------------------------------|------------------------------|------------------------------------|
| US OFAC         | `OFAC_DOWNLOAD_TEMPLATE`                | `/sdn.csv`, `/add.csv`, ...  | 302 → S3 (followed internally)     |
| US Non-SDN      | `US_NON_SDN_DOWNLOAD_TEMPLATE`          | `/CONS_PRIM.CSV`, ...        | 302 → S3 (followed internally)     |
| US CSL          | `US_CSL_DOWNLOAD_TEMPLATE` / `_URL`     | `/consolidated.csv`          | ~2 MB, Azure, frequently flaky     |
| EU CSL          | `EU_CSL_DOWNLOAD_URL`                   | `/eu_csl.csv`                | >2 MB, 48 h cache                  |
| UK Sanctions    | `UK_SANCTIONS_LIST_URL`                 | `/UK_Sanctions_List.csv`     | Optional                           |
| UN CSL          | `UN_CONSOLIDATED_LIST_URL`              | `/un_consolidated.xml`       | XML — use the correct env var!     |
| FinCEN 311      | `FINCEN_311_DOWNLOAD_URL`               | `/fincen_311.html`           | Small HTML page                    |

**Important**: US lists are CSV only. Feeding the UN XML URL into any `US_CSL_*` variable will produce exactly the parse error the cache was built to avoid.

## How the Cache Actually Works

1. Watchman starts and begins parallel downloads using the URLs you gave it.
2. nginx receives the request. For the 8 OFAC/Non-SDN files it returns a 302 from the origin — nginx intercepts it (`error_page 301 302 ... = @follow_redirect`) and performs the second fetch itself.
3. The final body is stored under the original stable filename (`/sdn_comments.csv`, `/CONS_ALT.CSV`, etc.) with a 48 h TTL.
4. On every subsequent request (including watchman restarts) you get a `cache:HIT` as long as the named volume still exists.

The `@follow_redirect` location and the 8 calling locations contain the large-buffer + long-timeout + retry hardening that makes cold-start success much more likely.

## Persistence Strategy (The Real Secret)

The named volume `cache-storage` mounted at `/var/cache/nginx` is the highest-leverage thing in this setup.

- A successful cold start populates the volume with complete copies.
- Every future `docker compose up` (without `-v`) is fast and almost entirely cache hits.
- In production, **never** delete this volume on routine deploys or restarts.

To force a completely cold start (useful for testing):

```bash
docker compose down -v
docker compose up -d --build --wait
```

## Outage Resilience & Stale Content Serving

This is the mechanism that protects watchman when government sources are completely unavailable for extended periods (the US CSL has had multi-day outages, for example).

### How it works

- Cached responses have a **freshness lifetime** controlled by `proxy_cache_valid`:
  - **48 hours** for the large/flaky lists: `/consolidated.csv`, `/eu_csl.csv`, and all 8 OFAC/Non-SDN files (`sdn*.csv`, `CONS_*.CSV`, etc.)
  - **24 hours** for smaller lists (the global default)
- After the freshness lifetime expires, the cached copy is marked **stale**.
- When the origin is down or returns errors, the directive `proxy_cache_use_stale error timeout ... http_5xx updating` tells nginx to **serve the stale copy** to the client (watchman) instead of failing the request.
- `proxy_cache_background_update on` lets nginx attempt to refresh the content in the background while still serving the old good version to watchman.
- The cache zone setting `inactive=7d` means a file remains on disk (and therefore eligible to be served stale) as long as it has been accessed at least once in the last 7 days.
- `proxy_ignore_headers Cache-Control Expires ...` is set globally, so the origin cannot force a shorter cache lifetime.

### Real-world example

If the US CSL (`/consolidated.csv`) went down for 3 days:

- A successful copy fetched before the outage would be served normally for the first 48 hours.
- After 48 hours it would be served as **stale** for the remaining time.
- Watchman would continue to receive a complete, valid CSV and would not crash or restart.
- The only ways this protection would be lost are:
  - The named `cache-storage` volume was deleted (`docker compose down -v` or equivalent).
  - The cached entry went completely untouched for more than 7 days.

This combination of longer TTLs on the important lists + aggressive stale serving + 7-day disk retention is one of the main reasons this cache setup exists.

## Customization

### Cache TTLs

Edit `docker/nginx-cache/nginx.conf`:

```nginx
# Global defaults
proxy_cache_valid 200 24h;

# Per-location overrides already present for the big files
location = /consolidated.csv { ... proxy_cache_valid 200 48h; ... }
location = /sdn_comments.csv { ... proxy_cache_valid 200 48h; ... }
# etc.
```

The cache zone itself uses `inactive=7d`.

See the **Outage Resilience & Stale Content Serving** section above for why the 48 h values on the large lists + 7-day inactive retention are important when origins are down for multiple days.

### Adding a New List

1. Add an exact `location = /your-file.csv { ... }` block in `nginx.conf`.
2. Set the corresponding watchman env var to point at `http://cache:8080/your-file.csv`.
3. (Optional) Mount your own `nginx.conf` for CI or advanced use.

Unknown paths return 404 — the cache is deliberately not an open proxy.

### Other Common Changes

- Change the resolver for air-gapped environments
- Adjust buffer sizes or timeouts for your network
- Add `proxy_ssl_verify on` + CA bundle if you need stricter TLS

After editing the bind-mounted config, run:

```bash
docker compose restart cache
```

## Testing

A pure-Go integration test (no external dependencies) exists:

```bash
make test          # full cold-start + log validation (can take up to 8 min)
make test-short    # unit tests only
```

It brings the stack up, waits for health, asserts that watchman reports PONG, and greps logs for both positive ("finished X download") and negative failure strings.

## Makefile Targets

| Target         | Description                              |
|----------------|------------------------------------------|
| `make up`      | Build cache + start both services        |
| `make down`    | Stop and remove containers + volume      |
| `make logs`    | Tail both services                       |
| `make logs-cache` / `logs-watchman` | Tail just one side                |
| `make ping`    | Quick health check against watchman      |
| `make test`    | Run the integration test                 |
| `make clean`   | Nuclear option (images + volumes)        |

## Troubleshooting

**First start is slow** — expected on a cold volume. 4–7 files are fetched from the internet. Subsequent starts should be seconds.

**Still seeing "unexpected EOF" or "max retries" on cold start?**
- This is almost always a transient origin failure during the initial parallel burst.
- The cache + named volume makes the *second* and later starts reliable.
- Capture `make logs-cache` and `make logs-watchman` around the failure window.

**IPv6 "Network unreachable" errors**
- The resolver is intentionally configured with `ipv6=off`. Some government hostnames still log AAAA attempts; the connection falls back to IPv4.

**XML inside a US CSL error**
- You set `US_CSL_DOWNLOAD_URL` (or the template) to the UN consolidated XML URL. US lists are CSV only. See the warning block in `docker-compose.yml`.

**Cache never hits**
- You are running with `-v` (volume removal) on every restart.
- The bind mount for `nginx.conf` is missing or wrong.

## Production Notes

- Mount the nginx config read-only from your own repository or config management.
- Keep the `cache-storage` volume for the lifetime of the deployment.
- Consider increasing `max_size` on the cache zone if you run many lists or very long TTLs.
- The healthcheck on the cache (`/health`) is intentionally trivial and fast.
- Watch the access log for `cache:MISS` rates after the initial population window.

## License

Apache 2.0 (same license as watchman).

## Contributing

Improvements to the nginx configuration, additional list mappings, better test coverage, or documentation are very welcome. Open an issue or PR.