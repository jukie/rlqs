# RLQS - Rate Limit Quota Service

A gRPC server implementing Envoy's [Rate Limit Quota Service](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/rate_limit_quota_filter) protocol. Envoy proxies connect via bidirectional streaming, report per-bucket request usage, and receive quota assignments in return.

## Quickstart

Run the RLQS server alongside Envoy in under 5 minutes.

### Prerequisites

- Docker and Docker Compose

### 1. Start the stack

```bash
cd examples/basic
docker compose up
```

This starts:
- **rlqs-server** on port `18081` (gRPC)
- **envoy** on port `10000` (HTTP) proxying to `httpbin.org`

### 2. Send traffic

```bash
# Matches the "api-users" bucket (header-based routing)
curl -H "x-user-class: api" http://localhost:10000/headers

# Matches the catch-all bucket (no header)
curl http://localhost:10000/get
```

### 3. Observe rate limiting

The RLQS server assigns each bucket a `requests_per_time_unit` quota (default: 100 RPS). Envoy enforces the assignment and reports usage back on a configurable interval.

```bash
# Watch RLQS server logs to see usage reports and quota assignments
docker compose logs -f rlqs-server
```

## Architecture

```
                    ┌─────────────────┐
  HTTP request ───▶ │   Envoy Proxy   │ ───▶ upstream
                    │                 │
                    │  RLQS filter:   │
                    │  - match bucket │
                    │  - enforce quota│
                    │  - report usage │
                    └───────┬─────────┘
                            │ gRPC bidi stream
                            ▼
                    ┌─────────────────┐
                    │   RLQS Server   │
                    │                 │
                    │  Engine:        │
                    │  - track usage  │
                    │  - assign quota │
                    └─────────────────┘
```

**Flow:**

1. Envoy's `rate_limit_quota` filter matches incoming requests to **buckets** using `bucket_matchers`.
2. On first match, Envoy opens a gRPC stream to the RLQS server and requests a quota assignment.
3. The RLQS server responds with a `RateLimitStrategy` (e.g., 100 requests/second).
4. Envoy enforces the assignment locally and periodically reports usage counts back.
5. The RLQS server can update assignments at any time by pushing new responses on the stream.

## Configuration Reference

### Config file

```yaml
server:
  grpc_addr: ":18081"      # gRPC listen address

engine:
  # Tokens added per reporting interval for the default token bucket strategy.
  # With reporting_interval=10s, 100 tokens = 10 tokens/second.
  default_tokens_per_fill: 100
  reporting_interval: "10s"  # Expected reporting interval from Envoy
```

### CLI flags

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | _(none)_ | Path to YAML config file. Without this, built-in defaults are used. |

### Environment variables

Environment variables override config file values:

| Variable | Default | Description |
|----------|---------|-------------|
| `RLQS_GRPC_ADDR` | `:18081` | gRPC listen address |
| `RLQS_DEFAULT_TOKENS_PER_FILL` | `100` | Tokens per reporting interval for default token bucket |
| `RLQS_REPORTING_INTERVAL` | `10s` | Reporting interval (Go duration string) |

**Precedence:** defaults < config file < environment variables.

### Health checks

The server registers the [gRPC Health Checking Protocol](https://github.com/grpc/grpc/blob/master/doc/health-checking.md):

```bash
grpcurl -plaintext localhost:18081 grpc.health.v1.Health/Check
```

Service names:
- `""` (empty) - overall server health
- `envoy.service.rate_limit_quota.v3.RateLimitQuotaService` - RLQS service

## Development

```bash
# Install the pre-commit hook (runs lint + tests before each commit)
make install-hooks

# Run lint and tests manually
make check
```

## Build

```bash
# Build binary
make build
# Output: bin/rlqs-server

# Run tests
make test

# Build Docker image
docker build -t rlqs-server .
```

## Run

```bash
# With defaults (listen on :18081, 100 tokens per interval)
./bin/rlqs-server

# With config file
./bin/rlqs-server -config config.yaml

# With environment overrides
RLQS_DEFAULT_TOKENS_PER_FILL=50 RLQS_GRPC_ADDR=":9090" ./bin/rlqs-server
```

## Examples

See [`examples/`](examples/) for ready-to-run setups:

| Example | Description |
|---------|-------------|
| [`basic`](examples/basic/) | Single Envoy + RLQS server with header-based bucket matching |
| [`multi-domain`](examples/multi-domain/) | Multiple Envoy frontends sharing one RLQS server with per-domain quotas |
| [`ha`](examples/ha/) | Two RLQS instances with shared Redis storage and Envoy failover |

## High Availability

RLQS supports running multiple stateless instances behind a load balancer with shared Redis storage. No distributed consensus is required -- quota assignments come from static policy config and usage tracking is best-effort. See [docs/ha.md](docs/ha.md) for the full architecture and failure recovery documentation.

## How Envoy's RLQS filter works

The `rate_limit_quota` filter in Envoy uses **bucket matchers** to classify requests. Each bucket gets an independent quota assignment from the RLQS server.

Key Envoy filter config fields:
- **`rlqs_server`** - gRPC cluster pointing to the RLQS server
- **`domain`** - Groups buckets by service/application
- **`bucket_matchers`** - Matcher tree that maps requests to buckets
- **`bucket_id_builder`** - Generates bucket IDs from request attributes (headers, paths, etc.)
- **`reporting_interval`** - How often Envoy reports usage per bucket
- **`no_assignment_behavior`** - What to do before the server assigns a quota (`ALLOW_ALL` or `DENY_ALL`)

See the [annotated envoy.yaml](examples/basic/envoy.yaml) for a working configuration with inline explanations.
