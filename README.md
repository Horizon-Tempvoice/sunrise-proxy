# <img src="assets/icon.svg" alt="" width="64" height="64"> Sunrise

Sunrise is an HTTP proxy for Discord's REST API that handles ratelimits for every client behind it. Point your bot processes at Sunrise instead of `https://discord.com`. Sunrise learns Discord's buckets from the response headers, holds each request until it can go out without a 429, and exports Prometheus metrics about all of it.

Sunrise was built for the Horizon bot, whose shards run in several processes and share one token. Nothing in it is specific to Horizon: any client that can change its API base URL works with it.

Several Sunrise nodes can run as a cluster. Every node accepts every request and passes it to the node that owns the request's bucket, so all nodes share one view of the ratelimits. Each node still sends to Discord from its own IP address.

Sunrise speaks plain HTTP and has no authentication for clients. Run it on a private network or bind it to loopback.

## Features

- Per-route buckets, learned from `X-RateLimit-Bucket` and kept apart per channel, guild, webhook and interaction
- A global limit per bot (50 requests per second by default, overridable per bot), shared across the cluster
- Ordered queues per bucket, with parallel requests once the bucket's limits are known
- Bot tokens, Bearer tokens and tokenless webhook requests
- Protection against Cloudflare's invalid request ban: token lock after a 401, local 404s for dead webhooks, a ten minute counter of invalid responses with warnings and an optional hard brake
- Clustering over gossip with no leader and no quorum. Nodes are found through a static list or DNS, gossip is encrypted, and requests between nodes are signed
- Any API version, any route, no route list to keep up to date
- Detailed Prometheus metrics and a Grafana dashboard in `grafana/sunrise.json`
- Logs that never contain tokens, as colored text or JSON

## Usage

Build and run with Docker Compose:

```sh
docker compose up -d --build
curl http://127.0.0.1:8080/api/v10/gateway
```

Or build the binary:

```sh
go build -o sunrise ./cmd/sunrise
./sunrise
```

Then change the client's API base URL from `https://discord.com/api` to `http://<sunrise-host>:8080/api`. The proxy accepts both normal request paths (`GET /api/v10/users/@me`) and absolute URLs as sent to an HTTP proxy (`GET http://host:8080/api/v10/users/@me`). Either way the request goes to `https://discord.com`, whatever host the URL names.

## Configuration

Sunrise reads environment variables, plus an optional `.env` file in its working directory (variables already set in the environment win). Invalid values stop the start with an error that names the variable. The most used ones:

| Variable | Default | Meaning |
|---|---|---|
| `SUNRISE_PORT` | `8080` | Proxy port |
| `SUNRISE_METRICS_PORT` | `9000` | Port for `/metrics`, `/healthz`, `/readyz` |
| `SUNRISE_BIND_IP` | `0.0.0.0` | Address both ports listen on |
| `SUNRISE_LOG_LEVEL` | `info` | `trace`, `debug`, `info`, `warn` or `error` |
| `SUNRISE_LOG_FORMAT` | `text` | `text` or `json` |
| `SUNRISE_REQUEST_TIMEOUT` | `15000` | Milliseconds to wait for Discord, not counting ratelimit waits |
| `SUNRISE_GLOBAL_RATELIMIT` | `50` | Default global requests per second per bot |
| `SUNRISE_BOT_RATELIMIT_OVERRIDES` | | `botid:limit,botid:limit` for bots with a raised global limit |
| `SUNRISE_CLUSTER_MEMBERS` | | Comma separated `host` or `host:port` list of other nodes |
| `SUNRISE_CLUSTER_DNS` | | DNS name that resolves to the nodes |
| `SUNRISE_CLUSTER_SECRET` | | Shared secret, required in cluster mode, at least 32 characters |

Every variable is described in [CONFIG.md](CONFIG.md).

## Behaviour

Sunrise collapses each request path into a route key: ids become placeholders, the top-level resource (channel, guild, webhook with its token, interaction) keeps its id. Each route key gets a queue. The first request on a new route goes out alone. Its response tells Sunrise the bucket's limit, how many requests remain and when the window resets, and from then on up to `remaining` requests run in parallel while the rest wait in order.

Discord reports the bucket hash in `X-RateLimit-Bucket`. Routes that report the same hash for the same top-level resource share one queue from then on. The hash alone is not enough, since Discord reuses it across channels and guilds.

Every response carries Discord's own count of remaining requests. Sunrise trusts the newest one and subtracts the requests sent after it. That works the same for fixed and sliding windows. When the window resets, the bucket fills up again.

A 429 goes back to the client unchanged. Sunrise does not retry. A 429 with scope `user` or `shared` pauses that bucket for `Retry-After`, a 429 with scope `global` pauses the whole bot. It is safe to retry a 429 right away, since the retry waits in the queue.

Requests without a token and the interaction endpoints do not count toward the global limit, because Discord does not count them either.

Clients that disconnect while waiting leave the queue and do not use a slot. Response bodies are streamed. Request bodies up to 1 MiB are read before the request waits, which is what lets Sunrise notice a disconnect. Larger bodies are streamed.

### Cloudflare protection

Discord bans an IP for a while once it causes 10,000 responses with status 401, 403 or 429 within ten minutes. 429s with scope `shared` do not count. Sunrise guards against that:

- A 401 for a bot or Bearer token during normal operation locks that token. Sunrise answers 401 itself from then on. By default the lock lasts until restart, `SUNRISE_TOKEN_LOCK_DURATION` makes it expire.
- A 404 on a webhook (not on an interaction) makes Sunrise answer 404 for that webhook itself for 24 hours. Discord asks clients not to use such webhooks again.
- Each node counts its invalid responses over the last ten minutes, logs warnings at 2,500, 5,000 and 7,500, and with `SUNRISE_ENABLE_HARD_BRAKE=true` answers every request with a generated 429 once the count reaches 9,500.

## Proxy specific responses

Responses that Sunrise generates itself carry `X-Sunrise-Generated: true` and an `X-Sunrise-Reason` header. Their bodies have the shape of Discord's error bodies.

| Status | Reason | When |
|---|---|---|
| 429 | `owner-unreachable` | The node that owns the bucket did not answer. The client should retry after one second |
| 429 | `cluster-joining` | The node has not finished joining the cluster yet |
| 429 | `hard-brake` | The hard brake is on and the invalid response count is at its threshold |
| 401 | `token-locked` | The token got a 401 earlier and is locked |
| 401 | `invalid-signature` | A request carried `X-Sunrise-*` headers without a valid signature |
| 404 | `webhook-404` | The webhook returned 404 earlier |
| 408 | `upstream-timeout` | Discord did not answer within `SUNRISE_REQUEST_TIMEOUT` |
| 502 | `upstream-error` | The connection to Discord failed |
| 400 | `path-traversal` | The path contains a `..` segment |
| 405 | `connect-not-supported` | The client sent `CONNECT` |

Generated 429s set `Retry-After: 1` and the body `{"message": "You are being rate limited.", "retry_after": 1, "global": false}`.

## Limitations

- Sunrise does not know a bot's real global limit, since Discord does not report it. Set `SUNRISE_BOT_RATELIMIT_OVERRIDES` for bots with a raised limit.
- Routes for creating and editing emojis are limited per guild in ways the headers do not show, so 429s can still happen there.
- No TLS. Run Sunrise on a private network, a WireGuard tunnel or loopback.
- Requests between nodes carry the client's `Authorization` header in plain HTTP. The signature covers method, path, query, time and node name, but not the body, so somebody who can read and replay traffic on the cluster network can resend a request for 30 seconds. Encrypt that network.
- REST only: no gateway, no voice, no caching, no retries.

## Clustering and high availability

Cluster mode starts when `SUNRISE_CLUSTER_MEMBERS` or `SUNRISE_CLUSTER_DNS` is set. If both are set, the list wins. Nodes gossip over port 7946 (TCP and UDP) with [memberlist](https://github.com/hashicorp/memberlist), an implementation of SWIM. The cluster has no leader and needs no quorum: during a network partition each side keeps working on its own. Gossip is encrypted with AES-256 using a key derived from `SUNRISE_CLUSTER_SECRET`.

`SUNRISE_CLUSTER_DNS` may resolve to several A or AAAA records, or be an SRV name starting with `_` (for example `_sunrise._tcp.example.org`). Sunrise resolves it again every 30 seconds and joins addresses it does not know yet, so nodes find each other after restarts and partitions.

Which node owns a request is decided by rendezvous hashing over the live members. For bot requests the key is the bot plus the top-level resource, so all routes of one channel or guild end up on one node. When a node leaves, only the keys it owned move. When a node joins, it takes over roughly its share. A request that arrives at another node is forwarded to the owner with an HMAC-SHA256 signature. Forwarded requests are never forwarded again. If the owner does not answer, the client gets a generated 429 and its retry lands on the new owner once the member list has settled.

Interaction callbacks and `GET /gateway` are always handled by the node that received them. Callbacks are sent once per interaction and do not count toward the global limit. `/gateway` needs no token, and Discord limits tokenless requests per IP.

The global limit of a bot is counted by the node that owns the bot. The other nodes take small batches of the current second's slots from it and spend them locally, so most requests cost no extra network call. If the owner is unreachable, each node falls back to `limit / members` per second until the owner answers again. A global 429 seen by any node pauses the bot on every node.

Changing members moves buckets to nodes that have not learned them yet, so expect a few extra probes right after a node joins or leaves. Restart nodes one at a time.

`docker-compose.clustered-example.yaml` runs three nodes that find each other through a shared DNS alias.

## Bearer tokens

Bearer tokens (OAuth2 user tokens) get their own queues. Sunrise keeps up to `SUNRISE_MAX_BEARER_COUNT` of them in an LRU and drops the least recently used. In a cluster, Bearer requests are spread by a hash of the token, and their global limit is always counted on the node that handles them.

## Metrics

Metrics are served on `/metrics` of the metrics port. Labels hold normalized routes and bot ids, never tokens or raw paths.

| Metric | Labels | Description |
|---|---|---|
| `sunrise_requests_seconds` | method, status, route, bot | Histogram of requests to Discord. `status` is the HTTP code, `429_shared` for 429s with scope `shared` |
| `sunrise_generated_responses_total` | reason | Responses Sunrise answered itself |
| `sunrise_errors_total` | kind | Errors while handling requests |
| `sunrise_open_connections` | method, route | Client requests currently open |
| `sunrise_routed_sent_total` | | Requests forwarded to the owner |
| `sunrise_routed_received_total` | | Requests received from other nodes |
| `sunrise_routed_failed_total` | | Forwards that failed |
| `sunrise_buckets` | | Buckets tracked on this node |
| `sunrise_bucket_queue_depth_max` | | Length of the longest bucket queue |
| `sunrise_requests_waiting` | stage | Requests waiting for a slot, `bucket` or `global` |
| `sunrise_ratelimit_wait_seconds` | stage | Histogram of time spent waiting for a slot |
| `sunrise_bucket_remaps_total` | | Routes moved onto a shared bucket |
| `sunrise_cluster_members` | | Nodes this node sees, itself included |
| `sunrise_forward_seconds` | | Histogram of forward time to the owner, waiting included |
| `sunrise_forward_errors_total` | reason | Failed forwards: `connect`, `timeout`, `other` |
| `sunrise_signature_rejected_total` | reason | Rejected signatures: `missing`, `malformed`, `stale`, `invalid`, `unexpected` |
| `sunrise_owner_changes_total` | | Recently used routing keys that moved to another node |
| `sunrise_global_lease_requests_total` | result | Global limit batches requested from the owner: `granted`, `empty`, `error` |
| `sunrise_global_lease_fallbacks_total` | | Times the local share was used because the owner was unreachable |
| `sunrise_invalid_requests_window` | | Invalid responses in the last ten minutes |
| `sunrise_invalid_responses_total` | status, scope | Invalid responses by status and scope |
| `sunrise_locked_tokens` | | Tokens locked after a 401 |
| `sunrise_webhook_404_cached` | | Webhooks answered with a local 404 |
| `sunrise_hard_brake_total` | | Requests stopped by the hard brake |
| `sunrise_build_info` | version, commit, go_version | Always 1 |

Go runtime and process metrics are exported as well.

## Health

- `GET /healthz` on the metrics port answers 200 while the process runs.
- `GET /readyz` answers 200 once the proxy listens and, in cluster mode, the node has joined. It answers 503 during shutdown.

The container image has no shell, so `sunrise healthcheck` checks `/readyz` from inside the container. The Dockerfile uses it as `HEALTHCHECK`.

On `SIGTERM` Sunrise reports not ready, leaves the cluster, finishes in-flight requests for up to `SUNRISE_SHUTDOWN_TIMEOUT` and stops.

## Profiling

With `SUNRISE_ENABLE_PPROF=true`, Go's profiling endpoints are served on the metrics port under `/debug/pprof/`.

## Releases

Pushing a tag such as `v1.2.3` runs the tests, publishes `ghcr.io/horizon-tempvoice/sunrise-proxy` for linux/amd64 and linux/arm64, and creates a GitHub release with generated notes. The image gets the tags `1.2.3`, `1.2`, `1` (skipped while the major version is 0), `latest` and the short commit. A tag with a suffix, such as `v1.3.0-rc.1`, gets `pre-release` instead of `latest`. Every push to `main` publishes `nightly` and `nightly-<commit>`.

Pin a version in production, for example `ghcr.io/horizon-tempvoice/sunrise-proxy:1.2.3`.

## License

GPL-3.0. See [LICENSE](LICENSE).
