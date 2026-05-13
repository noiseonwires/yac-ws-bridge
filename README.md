# YC Serverless Functions websocket tunnel/proxy

(unofficial alias: **Bridge to Freedom**)

A TCP and WebSocket tunnel via YC (using Serverless Functions and API Gateway, and there's also an experimental branch that uses QUIC protocol mechanics over WS) to bypass Russian internet censorship under strict whitelist filtering.

See [README_RU.md](README_RU.md) for documentation in Russian.

The tunnel comes in three variants:

1. Single-adapter variant (branch `one-adapter`): proxies WebSocket connections (VLESS with WS transport or XMPP-over-WebSockets, for example). Does not require modifying clients or installing extra software on the client device — just point your WebSocket URL at the serverless function. Works slowly and very unstably. See README_RU.md in the `one-adapter` branch for details.

2. Adapter + helper variant (branch `main`) — one component on the server side, one on the client side. Proxies arbitrary TCP connections and works much more stably and faster.

3. Experimental variant (branch `experimental`), similar to variant 2 but instead of a custom multiplexing/error-correction algorithm it uses QUIC wrapped in WebSocket messages.

In the adapter + helper variant:

Two Go binaries — **adapter** (on the target server side) and **helper** (on the client side) — each maintain an upstream WebSocket connection to the YC API Gateway. In "no relay" mode, data is sent **directly** through the YC WebSocket management API (gRPC `wsSend`), bypassing the Serverless Function on the data path. The Serverless Function is only needed so the client and server (helper and adapter) can find each other.

Under good conditions (adapter located in Russia near Yandex) you can squeeze out up to 20 Mbit/s.

There is also a cross-platform **MAUI application** (Android, iOS, Windows, macOS, and Linux via GTK4) as a GUI alternative to the Go helper — see [maui-client/README.md](maui-client/README.md).

> **Note:** v4 tunnels raw TCP streams. Path prefixes (available in the single-adapter v3 for WebSocket proxying) are not supported.

```
                              YC
                     ┌─────────────────────────┐
Client ──TCP──► Helper ──wsSend(gRPC)──► API Gateway ──WS──► Adapter ──TCP──► Target
                     │                         │
Client ◄──TCP── Helper ◄──WS────────── API Gateway ◄──wsSend(gRPC)── Adapter ◄──TCP── Target
                     │                         │
                     │    Cloud Function       │
                     │    (discovery only)     │
                     └─────────────────────────┘
```

## How it works

1. **Adapter** makes an outbound connection to the API Gateway at path `/_adapter` and authenticates with the Serverless Function (HELLO handshake). It receives its connection ID.
2. **Helper** connects at path `/_helper`, authenticates, and receives the adapter's connection ID. The Serverless Function also notifies the adapter of the helper's ID.
3. **Client** opens a TCP connection to the helper's port. The helper assigns a stream ID and sends an OPEN frame to the adapter via `wsSend`.
4. **Adapter** opens a TCP connection to the target service and replies with OPEN_OK.
5. Data flows both ways: TCP → helper → wsSend → adapter → TCP (and back).
6. When the TCP connection is closed on either side, a FIN frame is sent to close the corresponding stream.

All TCP streams are multiplexed over two upstream WebSocket connections (one in each direction).

### Reordering

In relay mode, frames belonging to the same stream may arrive out of order. The adapter automatically reorders incoming frames by `SeqID`: frames that arrive earlier than expected are buffered and delivered to the application in the correct order. There are no configuration parameters for this — the mechanism is always enabled in the adapter.

### Write coalescing

The `writeCoalescing` option implements an algorithm similar to TCP Nagle: small TCP reads are buffered and merged into a single DATA frame. This significantly reduces the number of `wsSend` / relay messages, which improves throughput and reduces load on YC. Data is sent either after `delayMs` elapses, or when the buffer reaches 32 KB — whichever comes first. With the Go helper, it is recommended to always enable this on the adapter side when using relay mode; without relay it is optional on both sides (try it — it may be better with or without). In the MAUI app it is buggy.

**Important**: one adapter — one function — one client!
Multiple clients to the same function/adapter result in undefined behavior (everything breaks). To support multiple clients you'd need to slightly redesign the protocol and implementation, but I can't be bothered.

### Relay mode

If the helper cannot reach the `wsSend` gRPC endpoint (the YC API — for example on a restricted network), set `wsApi.relay: true`. The helper sends data through its upstream WebSocket and the Serverless Function relays it to the adapter. The reverse path (adapter → helper) still uses `wsSend` directly. Relay mode is slower and less stable for the same reasons as the single-adapter (one-branch) variant, but is still more stable thanks to proper multiplexing.

---

## Important

How long until Yandex bans you for this — no idea.

Recommendations:

- Use this only for proxying the most important low-traffic services (for example, tunnel through to a SOCKS proxy and plug it into Telegram as 127.0.0.1). Don't abuse it by pushing large amounts of data.

- Before uploading the serverless function code, run it through any JavaScript obfuscator (a couple of times, even). In the serverless function, adapter, and helper code, replace all the default paths (`/_upstream`, `/_helper`, `/_conn-ids`) with random ones of your own.

### Also important

If you use a proxy client that works as a TUN, you must add an exclusion for the helper process, otherwise you'll get an infinite loop and nothing will work. The mobile app (MAUI) writes endpoint domains and IP addresses to the log on connect — you can add those to exclusions if per-process exclusions aren't available. That said, in the Happ client it didn't work for me even with that (but it works fine without TUN, e.g. for the same TG).

---

## Adapter

Goes on some VPS. Ideally also in Russia, close to Yandex; from there proxy onward wherever and however you like.

### Build

Requires Go 1.21+.

```bash
cd adapter
go build -o adapter ./cmd/adapter
```

### Configuration

Create `adapter.config.yaml`:

```yaml
bridge:
  url: "wss://<api-gateway-domain>/_adapter"
  authToken: "<shared-secret>"
  reconnect:
    initialDelayMs: 1000
    maxDelayMs: 30000
    backoffMultiplier: 2
  pingIntervalMs: 30000

target:
  address: "127.0.0.1:9090"

http:
  listenPort: 3001

writeCoalescing:
  enabled: true
  delayMs: 50

wsApi:
  mode: "grpc"

logging:
  level: "info"
```

| Key | Description |
|------|----------|
| `bridge.url` | WebSocket URL of the API Gateway endpoint for the adapter |
| `bridge.authToken` | Shared secret (must match `AUTH_TOKEN` in the Cloud Function) |
| `bridge.reconnect` | Exponential backoff for upstream reconnects |
| `bridge.pingIntervalMs` | PING interval to prevent idle disconnect (keep under 10 min) |
| `target.address` | TCP address of the target service |
| `http.listenPort` | HTTP port for the `/conn-ids` endpoint (used by the Cloud Function on cold start) |
| `writeCoalescing.enabled` | Coalesce small packets into a single frame (analogous to Nagle's algorithm). Reduces the number of `wsSend` calls and increases throughput |
| `writeCoalescing.delayMs` | Maximum buffering delay before sending (ms). Data is sent earlier if the buffer reaches 32 KB. Recommended: 10–100 ms |
| `wsApi.mode` | `grpc` (the only option in v4) |

### Run

```bash
./adapter adapter.config.yaml
```

### HTTP endpoints

| Endpoint | Method | Description |
|-----------|-------|----------|
| `/conn-ids` | GET | Returns `{"adapterConnId":"...","helperConnId":"..."}`. Authorization: `Bearer <authToken>`. |

---

## Helper

Goes on the client device.

Besides the console Go version, there is also a nice app (.NET MAUI for Windows, macOS, Android, iOS) with the same functionality. See the `maui-client` folder.

### Build

```bash
cd adapter-and-helper
go build -o helper ./cmd/helper
```

### Configuration

Create `helper.config.yaml`:

```yaml
bridge:
  url: "wss://<api-gateway-domain>/_helper"
  authToken: "<shared-secret>"
  reconnect:
    initialDelayMs: 1000
    maxDelayMs: 30000
    backoffMultiplier: 2
  pingIntervalMs: 30000

listen:
  address: "127.0.0.1:1080"

writeCoalescing:
  enabled: true
  delayMs: 50

wsApi:
  mode: "grpc"
  relay: false

logging:
  level: "info"
```

| Key | Description |
|------|----------|
| `listen.address` | TCP address to listen for client connections |
| `writeCoalescing.enabled` | Enable small-packet coalescing (see the adapter section above) |
| `writeCoalescing.delayMs` | Buffering delay before sending (ms). Recommended: 10–100 ms |
| `wsApi.relay` | `true` = send data through the upstream WS (the Cloud Function relays). `false` = send via gRPC `wsSend` directly. |

### Run

```bash
./helper helper.config.yaml
```

Clients connect to `listen.address` over plain TCP. Each connection is tunneled to the target service.

---

## Deploying the Serverless Function (YC)

### Prerequisites

- A YC account with billing enabled
- Installed and configured `yc` CLI (`yc init`)
- A folder in the cloud for the project

### 1. Create a service account

```bash
yc iam service-account create --name bridge-sa

SA_ID=$(yc iam service-account get bridge-sa --format json | jq -r .id)
FOLDER_ID=$(yc config get folder-id)

yc resource-manager folder add-access-binding $FOLDER_ID \
  --role serverless.functions.invoker \
  --subject serviceAccount:$SA_ID

yc resource-manager folder add-access-binding $FOLDER_ID \
  --role api-gateway.websocketBroadcaster \
  --subject serviceAccount:$SA_ID
```

### 2. Create and deploy the Serverless Function

```bash
cd bridge-cloud
zip -r bridge-function.zip index.js package.json
```

```bash
yc serverless function create --name bridge-fn
FUNCTION_ID=$(yc serverless function get bridge-fn --format json | jq -r .id)
```

Deploy a version:

```bash
yc serverless function version create \
  --function-name bridge-fn \
  --runtime nodejs18 \
  --entrypoint index.handler \
  --memory 128m \
  --execution-timeout 10s \
  --concurrency 16 \
  --source-path bridge-function.zip \
  --service-account-id $SA_ID \
  --environment "AUTH_TOKEN=<your-shared-secret>,ADAPTER_URL=<adapter-http-url>"
```

| Variable | Required | Description |
|------------|-------------|----------|
| `AUTH_TOKEN` | Yes | Shared secret (same value as `bridge.authToken`) |
| `ADAPTER_URL` | Yes | HTTP(S) URL of the adapter (e.g. `https://your-server:3001`). Used to fetch `/conn-ids` on cold start; needed to restore state across multiple instances. |

### 3. Create the API Gateway

Edit `bridge-cloud/spec.yaml` — replace the two placeholders:

- `${FUNCTION_ID}` — the function ID from step 2
- `${SERVICE_ACCOUNT_ID}` — the service account ID from step 1

Then create the gateway:

```bash
yc serverless api-gateway create \
  --name bridge-gw \
  --spec spec.yaml
```

Get the gateway domain:

```bash
GW_DOMAIN=$(yc serverless api-gateway get bridge-gw --format json | jq -r .domain)
echo "Gateway: wss://$GW_DOMAIN"
```

### 4. Configure and run the adapter

Set `bridge.url` in `adapter.config.yaml` to `wss://<GW_DOMAIN>/_adapter`.

```bash
cd adapter-and-helper
go build -o adapter ./cmd/adapter
./adapter adapter.config.yaml
```

### 5. Configure and run the helper

Set `bridge.url` in `helper.config.yaml` to `wss://<GW_DOMAIN>/_helper`.

```bash
cd adapter-and-helper
go build -o helper ./cmd/helper
./helper helper.config.yaml
```

### 6. Connect clients

Point any TCP client at the helper's listen address:

```bash
# Example: proxy SSH through the tunnel
ssh -o ProxyCommand="nc 127.0.0.1 1080" user@target
```

### 7. View Serverless Function logs

View Cloud Function logs:

```bash
yc logging read --folder-id $(yc config get folder-id) --follow
```

---

## YC limits

| Limit | Value |
|-------|----------|
| Maximum WebSocket connection lifetime | 60 minutes |
| Idle timeout (no messages) | 10 minutes |
| Maximum message size | 128 KB |
| Maximum frame size | 32 KB |
| Function execution timeout | 10 s (configurable) |

Set `pingIntervalMs` below 10 minutes to avoid idle disconnects.

## License

WTFPL, see [LICENSE](LICENSE).
It's just a PoC. There will be no further development, support, or answers to questions.