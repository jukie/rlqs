# High Availability

RLQS supports multi-instance deployment for high availability. Two or more stateless instances run behind a Kubernetes Service (or any L4 load balancer), sharing usage state through Redis.

## Architecture

```
                        ┌──────────────────┐
      K8s Service /     │  Load Balancer   │
      L4 LB             │  (round-robin)   │
                        └───┬──────────┬───┘
                            │          │
                    ┌───────▼──┐  ┌────▼──────┐
                    │  RLQS-1  │  │  RLQS-2   │    Stateless instances
                    │          │  │           │    (same config)
                    └───┬──────┘  └────┬──────┘
                        │              │
                        ▼              ▼
                    ┌──────────────────────┐
                    │       Redis          │    Shared usage counters
                    │  (HINCRBY atomic)    │
                    └──────────────────────┘
```

Each Envoy proxy opens a gRPC stream to the RLQS service. The load balancer distributes streams across instances. All instances read the same static policy configuration and share usage counters in Redis.

## Why Consensus Is Not Needed

RLQS instances do **not** require distributed consensus (no Raft, no leader election, no coordination). Two properties make this possible:

### 1. Strategies From Static Configuration

Rate limit strategies (token bucket parameters, RPS limits, assignment TTLs) are derived entirely from the static policy configuration. Every instance loads the same config file and produces identical quota assignments for the same bucket. There is no dynamic state that influences *what* rate limit to assign -- only *whether* the bucket has been seen.

This means any instance can respond to any Envoy's usage report with the correct quota assignment, independently.

### 2. Usage Tracking Is Best-Effort

Usage counters track how many requests have been allowed or denied per bucket. These counters serve two purposes:
- **Observability**: Operators can see aggregate traffic patterns via Prometheus metrics.
- **Future adaptive engines**: A future engine could adjust quotas based on observed usage.

In both cases, approximate counts are acceptable. Redis `HINCRBY` provides atomic increments, so concurrent updates from multiple instances are safe and produce correct totals. But even if a few counts are lost (e.g., during Redis failover or circuit breaker activation), the rate limiting behavior is unaffected because the quota assignments themselves come from static config, not from usage data.

**In short**: the control plane (quota decisions) is stateless and deterministic. The data plane (usage tracking) is shared but best-effort. No coordination is needed between instances.

## Failure Recovery

### Instance Crash

When an RLQS instance crashes or is terminated:

1. **Envoy detects stream disconnection** via gRPC keepalive or TCP reset.
2. **Envoy's `expired_assignment_behavior` activates** -- typically `ALLOW_ALL`, so traffic continues to flow.
3. **Envoy reconnects** to the RLQS service. The load balancer routes the new stream to a healthy instance.
4. **The new instance processes the first usage report** and sends back a quota assignment.
5. **Normal operation resumes.** Usage counters in Redis are rebuilt from incoming reports.

The gap between crash and reconnection is bounded by Envoy's gRPC reconnect backoff (typically seconds). During this window, Envoy falls back to its `no_assignment_behavior` or `expired_assignment_behavior`.

### Redis Failure

When Redis becomes unreachable:

1. **The circuit breaker trips** on the first Redis error.
2. **Each instance falls back to in-memory storage** (fail-open). Rate limiting continues with per-instance local counters.
3. **The circuit breaker retries Redis** every 5 seconds (configurable).
4. **On recovery**, the circuit breaker resets and subsequent writes go to Redis. In-memory fallback data is not synced back -- counters restart from the next reports.

During Redis downtime, each instance tracks usage independently. Global counts are approximate but rate limiting is unaffected since quota assignments come from static config.

### Rolling Deployment

During a rolling update (e.g., `kubectl rollout restart`):

1. **Kubernetes sends SIGTERM** to the old pod.
2. **The RLQS server initiates graceful shutdown**: stops accepting new streams, drains active streams with a 10-second timeout.
3. **Envoy detects stream closure** and reconnects to remaining healthy instances.
4. **The new pod starts**, passes readiness checks, and begins receiving streams.

The `PodDisruptionBudget` (minAvailable: 1) ensures at least one instance is always running.

### Network Partition

If an instance loses connectivity to Redis but can still serve gRPC:

1. The circuit breaker trips and the instance falls back to in-memory storage.
2. The instance continues serving quota assignments from static config.
3. Usage tracking is temporarily per-instance (not globally aggregated).
4. When connectivity restores, the circuit breaker recovers and Redis usage tracking resumes.

## Configuration

Enable Redis storage for multi-instance deployment:

```yaml
storage:
  type: redis
  redis:
    addr: "redis:6379"
    pool_size: 10
```

Or via environment variables:

```bash
RLQS_STORAGE_TYPE=redis
RLQS_STORAGE_REDIS_ADDR=redis:6379
RLQS_STORAGE_REDIS_POOL_SIZE=10
```

All instances must use the same Redis instance (or cluster) and the same policy configuration.

## Deployment Checklist

- [ ] All instances use `storage.type: redis` pointing to the same Redis
- [ ] All instances load the same policy configuration (via ConfigMap or identical config files)
- [ ] Envoy's `expired_assignment_behavior` is set to `ALLOW_ALL` for fail-open during reconnection
- [ ] Redis is deployed with persistence or replication appropriate to your durability requirements
- [ ] `PodDisruptionBudget` is configured with `minAvailable: 1` (or appropriate value)
- [ ] gRPC health checks are configured for readiness probes

## Example

See [`examples/ha/`](../examples/ha/) for a working multi-instance setup using Docker Compose with 2 RLQS instances, Redis, and Envoy.
