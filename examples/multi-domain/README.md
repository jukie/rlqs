# Multi-Domain Example: Shared RLQS Server

Two Envoy frontends sharing a single RLQS server with separate domains for independent quota tracking.

## Architecture

```
                                    ┌────────────────────┐
  Public traffic ──▶ public-envoy   │ domain: public-api │──┐
                     (port 10000)   └────────────────────┘  │   ┌─────────────┐
                                                             ├──▶│ rlqs-server │
                                    ┌────────────────────────┐  │   (port 18081)│
  Internal traffic ▶ internal-envoy │ domain: internal-svc   │──┘   └─────────────┘
                     (port 10001)   └────────────────────────┘
```

## Bucket layout

**Public API** (`public-api` domain):
- `authenticated` - requests with `Authorization` header (fail-open)
- `anonymous` - everything else (**fail-closed** - denied until RLQS assigns quota)

**Internal Services** (`internal-services` domain):
- `high-priority` - requests with `x-priority: high` header
- `normal` - everything else

Both domains share the same RLQS server but get independent quota assignments.

## Run

```bash
docker compose up
```

## Test

```bash
# Public: authenticated request (allowed immediately)
curl -H "Authorization: Bearer token123" http://localhost:10000/headers

# Public: anonymous request (denied until RLQS assigns quota)
curl http://localhost:10000/get

# Internal: high-priority request
curl -H "x-priority: high" http://localhost:10001/headers

# Internal: normal request
curl http://localhost:10001/get
```

## Key concept: domains

The `domain` field in Envoy's RLQS filter config groups buckets. The RLQS server receives the domain in every usage report. This lets you:

- Run a single RLQS server for multiple services
- Apply different quota policies per domain
- Track usage independently for each domain
