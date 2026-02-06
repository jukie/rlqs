# HA Example: Multi-Instance RLQS

This example runs two RLQS instances behind an Envoy proxy, sharing usage state through Redis.

## What It Demonstrates

- Two stateless RLQS instances with identical configuration
- Shared Redis backend for usage counter aggregation
- Envoy load-balancing gRPC streams across both instances
- gRPC health checks for automatic failover
- Fail-open behavior when an instance is unavailable

## Components

| Service | Port | Description |
|---------|------|-------------|
| `rlqs-1` | 18081 (internal) | First RLQS instance |
| `rlqs-2` | 18081 (internal) | Second RLQS instance |
| `redis` | 6379 | Shared usage counter storage |
| `envoy` | 10000 | HTTP proxy with RLQS filter |

## Run

```bash
docker compose up
```

## Test

```bash
# Send requests through Envoy
curl -H "x-user-class: api" http://localhost:10000/headers
curl http://localhost:10000/get

# Verify both instances receive traffic (check logs)
docker compose logs rlqs-1
docker compose logs rlqs-2
```

## Test Failover

```bash
# Stop one instance
docker compose stop rlqs-2

# Traffic continues through rlqs-1
curl http://localhost:10000/get

# Restart it
docker compose start rlqs-2
```

Envoy detects the downed instance via gRPC health checks and routes streams to the remaining healthy instance. See [docs/ha.md](../../docs/ha.md) for the full failure recovery documentation.
