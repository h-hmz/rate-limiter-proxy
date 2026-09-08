# rate-limiter-proxy

A reverse proxy that applies rate limiting before requests reach your app.

For each request that arrives at the proxy, it works out a **key** (the client IP, or an API key header), asks the **store** whether that key still has quota, and then either forwards the request to your app or returns a 429 response.

Built on [h-hmz/rate-limiter](https://github.com/h-hmz/rate-limiter).

## Quick start

```sh
export RL_APP_PORT=8080        # your app
export RL_ALGORITHM=tokenbucket
export RL_RATE=100             # tokens per second
export RL_BURST=200            # bucket size

go run .
```

```sh
curl -H 'X-Forwarded-For: 203.0.113.9' http://localhost:15001/
curl http://localhost:15090/healthz
curl http://localhost:15090/metrics | grep ratelimit
```

Rate limit responses carry `X-RateLimit-Limit`, `X-RateLimit-Remaining`, and `Retry-After`.

## Configuration

### Required

| Variable | Description |
| --- | --- |
| `RL_APP_PORT` | Port of the app to forward to, on `127.0.0.1`. |
| `RL_ALGORITHM` | `tokenbucket` or `fixedwindow`. |
| `RL_RATE` | `tokenbucket`: tokens per second. `fixedwindow`: requests per window, must be an integer. |
| `RL_BURST` | Bucket size. Required for `tokenbucket` only. |
| `RL_WINDOW` | Window length, a Go duration such as `30s`. Required for `fixedwindow` only. |

### Optional

| Variable | Default | Description |
| --- | --- | --- |
| `RL_LISTEN_PORT` | `15001` | Port the proxy listens on. |
| `RL_METRICS_PORT` | `15090` | Port for `/metrics` and `/healthz`. Must differ from `RL_LISTEN_PORT` and `RL_APP_PORT`. |
| `RL_STORE` | `memory` | `memory` or `redis`. |
| `RL_REDIS_ADDR` | | `host:port`. Required when `RL_STORE=redis`. |
| `RL_REDIS_PASSWORD` | | Redis password, if any. |
| `RL_KEY` | `ip` | What quota is counted against: `ip` or `apikey`. |
| `RL_KEY_HEADER` | `X-API-Key` | Header read when `RL_KEY=apikey`. |
| `RL_FAIL_OPEN` | `true` | What an unusable store means. See below. |
| `RL_LOG_FORMAT` | `json` | `json` or `text`. |

### Picking a key

`RL_KEY=ip` reads `X-Forwarded-For` and rejects requests that lack it. **This assumes the proxy sits behind an ingress that overwrites the header**, so clients cannot forge it. Do not expose this proxy directly to the internet in `ip` mode.

`RL_KEY=apikey` reads whatever header `RL_KEY_HEADER` names.

## Admin endpoints

`/metrics` and `/healthz` are served on their own port so they are never rate limited and cannot shadow routes of the same name in your app.

`/healthz` is liveness only. It reports that the process is up and serving, and says nothing about the store, because a store failure means fail open rather than unhealthy.

`/metrics` exposes `ratelimit_requests_total` (labelled by outcome) and `ratelimit_latency_seconds` through a Prometheus exporter.

## RL_FAIL_OPEN: Behavior on store failure

A shared store is a dependency that can go down, so you have to configure what happens in this scenario.

With `RL_FAIL_OPEN=true` (the default), a request is allowed when the store cannot answer. Quota stops being enforced, but traffic keeps flowing.

Set `RL_FAIL_OPEN=false` to reject requests when the store is unusable. That trades availability for enforcement, which is the right call if the limit protects something more important than uptime.

The proxy determines if the store is reachable by probing it on its own schedule. After 3 consecutive failed probes, all requests skip it entirely instead of waiting. One healthy probe restores enforcement. Only the transitions (open⇔closed) are logged, so a single outage produces two lines rather than one per each failed request.

**Startup is different.** If the store is unreachable at boot, it refuses to start rather than running unprotected forever. This is to protect against scenarios where there is a typo in `RL_REDIS_ADDR`. If it really is an outage, your orchestrator's restarts act as retries.
