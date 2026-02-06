# Basic Example: Envoy + RLQS Server

Single Envoy proxy with the RLQS filter connected to the RLQS server.

## What's in this example

| File | Purpose |
|------|---------|
| `docker-compose.yaml` | Runs both services |
| `envoy.yaml` | Annotated Envoy config with `rate_limit_quota` filter |
| `rlqs-config.yaml` | RLQS server config (100 RPS default) |

## Bucket layout

```
Incoming request
       │
       ▼
  Has "x-user-class: api" header?
       │
   ┌───┴───┐
  yes      no
   │        │
   ▼        ▼
 "api-users"  "catch-all"
  bucket       bucket
```

Both buckets receive 100 RPS quota from the RLQS server. Before the first assignment arrives, both buckets allow all traffic (`ALLOW_ALL`).

## Run

```bash
docker compose up
```

## Test

```bash
# Hits the "api-users" bucket
curl -H "x-user-class: api" http://localhost:10000/headers

# Hits the "catch-all" bucket
curl http://localhost:10000/get

# Check RLQS server logs for usage reports
docker compose logs rlqs-server

# Check Envoy stats for rate limiting
curl http://localhost:9901/stats | grep rate_limit_quota
```

## Customize

**Change the rate limit:** Edit `rlqs-config.yaml` and set `default_rps` to your desired value, then restart:

```bash
docker compose restart rlqs-server
```

**Add a new bucket:** Add another matcher entry in `envoy.yaml` under `bucket_matchers.matcher_list.matchers`. Each matcher needs a `predicate` (what to match) and an `on_match.action` with `bucket_id_builder` (what to call it).
