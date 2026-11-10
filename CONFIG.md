# Configuration

All variables are optional, except `SUNRISE_CLUSTER_SECRET` in cluster mode. Durations are whole milliseconds. Booleans accept `true`/`false` (also `1`/`0`, `yes`/`no`).

Sunrise also reads a `.env` file from its working directory if one exists. A variable that is already set in the environment wins over the file. At startup Sunrise logs which variables were set (names only) and the effective value of every setting, with the cluster secret redacted.

## Listeners

### SUNRISE_PORT
Port for proxied requests. Default `8080`.

### SUNRISE_METRICS_PORT
Port for `/metrics`, `/healthz`, `/readyz` and, if enabled, `/debug/pprof/`. Must differ from `SUNRISE_PORT`. Default `9000`.

### SUNRISE_BIND_IP
Address both ports listen on, and the gossip listener in cluster mode. `127.0.0.1` keeps everything on loopback, a private address such as `10.0.0.2` keeps it on that network. Default `0.0.0.0`.

### SUNRISE_ENABLE_METRICS
Serve the metrics port. Default `true`.

### SUNRISE_ENABLE_PPROF
Serve Go's profiling endpoints under `/debug/pprof/` on the metrics port. Needs `SUNRISE_ENABLE_METRICS=true`. Default `false`.

## Requests to Discord

### SUNRISE_REQUEST_TIMEOUT
How long to wait for Discord to answer, in milliseconds. Time spent waiting for a ratelimit slot does not count. When it runs out, the client gets a generated 408. Default `15000`.

### SUNRISE_DISABLE_HTTP_2
Talk HTTP/1.1 to Discord. Set to `false` to allow HTTP/2. Default `true`.

### SUNRISE_OUTBOUND_IP
Local address for connections to Discord, for hosts with several public addresses. Example: `203.0.113.10`. Default: chosen by the operating system.

## Ratelimits

### SUNRISE_GLOBAL_RATELIMIT
Global limit for a bot without an override, and for each Bearer token. Discord's default is 50 per second. Sunrise allows at most this many requests in any 1.1 seconds, which leaves room for network jitter. Default `50`.

### SUNRISE_BOT_RATELIMIT_OVERRIDES
Global limits for bots that Discord gave a higher limit. A comma separated list of bot user id and limit, without spaces. Use the bot's user id, which Sunrise reads from the first part of the token. Example: `392827169497284619:100,227115752396685313:80`.

### SUNRISE_ALLOW_CONCURRENT_REQUESTS
Once a bucket's limits are known, send up to `remaining` requests at once instead of one at a time. Ordering is kept either way. Default `true`.

### SUNRISE_MAX_BEARER_COUNT
How many Bearer tokens keep their queues at once. The least recently used token is dropped first. A request that is already waiting is never interrupted. Default `1024`.

## Cloudflare protection

### SUNRISE_DISABLE_401_LOCK
Do not lock tokens after a 401. The 401 is still logged. Default `false`.

### SUNRISE_TOKEN_LOCK_DURATION
How long a token stays locked after a 401, in milliseconds. While locked, Sunrise answers 401 itself. `0` keeps the lock until Sunrise restarts. Default `0`.

### SUNRISE_WEBHOOK_404_DURATION
How long Sunrise answers 404 itself for a webhook that returned 404, in milliseconds. Up to 10,000 webhooks are remembered. Default `86400000` (24 hours).

### SUNRISE_INVALID_REQUEST_WARN_THRESHOLDS
Counts of invalid responses (401, 403, 429 except scope `shared`) within ten minutes at which a warning is logged. Ascending, comma separated. Discord bans the IP at 10,000. Default `2500,5000,7500`.

### SUNRISE_ENABLE_HARD_BRAKE
Answer every request with a generated 429 while the invalid response count of this node is at or above `SUNRISE_HARD_BRAKE_THRESHOLD`. Default `false`.

### SUNRISE_HARD_BRAKE_THRESHOLD
Invalid responses within ten minutes at which the hard brake engages. Default `9500`.

## Logging

### SUNRISE_LOG_LEVEL
`trace`, `debug`, `info`, `warn` or `error`. `trace` logs every request and the gossip internals. Default `info`.

### SUNRISE_LOG_FORMAT
`text` for aligned lines (colored on a terminal) or `json` for log shipping. Default `text`.

## Shutdown

### SUNRISE_SHUTDOWN_TIMEOUT
After `SIGTERM`, how long in-flight requests get to finish, in milliseconds. Default `20000`.

## Cluster

Cluster mode starts when `SUNRISE_CLUSTER_MEMBERS` or `SUNRISE_CLUSTER_DNS` is set. Without either, Sunrise runs standalone.

### SUNRISE_CLUSTER_MEMBERS
Comma separated list of other nodes, as `host` or `host:port`. The port is the gossip port and defaults to `SUNRISE_CLUSTER_PORT`. The list does not need every node, gossip finds the rest. Wins over `SUNRISE_CLUSTER_DNS`. Example: `10.0.0.2,10.0.0.3:7947`.

### SUNRISE_CLUSTER_DNS
DNS name that resolves to the nodes. A name with A or AAAA records uses `SUNRISE_CLUSTER_PORT` for every address, so all nodes need the same gossip port. A name starting with `_` is looked up as an SRV record, which carries a port per node. Example: `sunrise.internal` or `_sunrise._tcp.example.org`.

### SUNRISE_CLUSTER_DNS_INTERVAL
How often discovery runs again to find new or restarted nodes, in milliseconds. Applies to the member list as well. Default `30000`.

### SUNRISE_CLUSTER_PORT
Gossip port, TCP and UDP. Must differ from the proxy and metrics ports. Default `7946`.

### SUNRISE_CLUSTER_SECRET
Shared secret of the cluster, at least 32 characters. Sunrise derives the gossip encryption key and the request signing key from it. Generate one with `openssl rand -hex 32`. Required in cluster mode.

### SUNRISE_CLUSTER_ADVERTISE_ADDR
Address other nodes should use to reach this one, when the address Sunrise detects is wrong (for example behind NAT, or to force the WireGuard address). Default: detected.

### SUNRISE_NODE_NAME
Name of this node in the cluster. Must be unique. Default: the hostname.
