# Policy-Based Rate Limiting Example

This example demonstrates the policy-based rate limiting feature that allows different rate limits for different domains and bucket keys.

## Configuration

The `rlqs-config.yaml` defines multiple policies that are matched in order:

1. **API endpoints** (`domain_pattern: "^api\\."`) - 1000 RPS with 20s TTL
2. **Premium users** (`bucket_key_pattern: "tier\x00premium"`) - 5000 RPS with 30s TTL
3. **Internal services** (`domain_pattern: "^internal\\."`) - 10000 RPS with 60s TTL
4. **Default policy** - 100 RPS with 20s TTL (for everything else)

## How It Works

The `PolicyEngine` evaluates each usage report against the configured policies:

1. For each bucket, it checks policies in order from top to bottom
2. The first policy whose `domain_pattern` AND `bucket_key_pattern` both match is selected
3. If no policy matches, the default policy is used
4. The matched policy's RPS and TTL are applied to the bucket

### Pattern Matching

- **domain_pattern**: Regular expression matched against the domain string
- **bucket_key_pattern**: Regular expression matched against the canonical bucket key
- Empty patterns match all values

### Bucket Key Format

Bucket keys are canonical strings with format: `key1\x00val1\x1ekey2\x00val2`

For example, a bucket with `{"tier": "premium", "user": "alice"}` becomes:
```
tier\x00premium\x1euser\x00alice
```

To match premium users, use pattern: `tier\x00premium`

## Running the Example

```bash
# Start the server with policy config
go run cmd/rlqs-server/main.go -config examples/policy/rlqs-config.yaml
```

## Benefits

- **Per-domain policies**: Different rate limits for different services
- **Per-bucket policies**: Custom limits based on user tier, region, etc.
- **Flexible matching**: Use regex patterns for complex rules
- **Fallback behavior**: Default policy ensures all buckets get a limit
- **No code changes**: Add/modify policies via YAML configuration
