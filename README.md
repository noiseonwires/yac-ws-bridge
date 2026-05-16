# YC Serverless Functions websocket tunnel/proxy

(unofficial alias: **Bridge to Freedom**)

A TCP and WebSocket tunnel via YC (using Serverless Functions and API Gateway, and there's also an experimental branch that uses QUIC protocol mechanics over WS) to bypass Russian internet censorship under strict whitelist filtering.

See [README_RU.md](README_RU.md) for documentation in Russian.

## What's new — 2026-05-15

This release is heavily optimised. The helper now reorders out-of-order frames on receive, pre-registers streams before sending `OPEN` (so the first DATA packets after `OPEN_OK` are never dropped), uses an async per-stream write queue so a slow local consumer no longer stalls every other stream, and does a graceful half-close on `FIN` so HTTP responses are no longer truncated. In practice: connections are established noticeably faster and the tunnel is significantly more stable, especially on mobile.
Also, multiple clients (helpers) can now connect to the same adapter/function simultaneously without interfering with each other. The previous "one adapter — one function — one client" limitation no longer applies.

## What's new — 2026-05-16

The adapter's HTTP endpoint path is now configurable via the new `http.path` setting in `adapter.config.yaml` (default `/conn-ids`), so you can rename it to something non-fingerprintable without editing source code. Correspondingly, the cloud function env var `ADAPTER_URL` has been renamed to `HTTP_URL` and its format has changed: it now expects the **full** URL of the adapter's endpoint (including the path), e.g. `https://your-server:8080/conn-ids`, instead of just the HTTP base. If you upgrade an existing deployment, update both the adapter config and the function's env variable. See [Customizing endpoint paths](#customizing-endpoint-paths) for details.

## Versions

The tunnel comes in four variants:

1. Single-adapter variant (branch `one-adapter`): proxies WebSocket connections (VLESS with WS transport or XMPP-over-WebSockets, for example). Does not require modifying clients or installing extra software on the client device — just point your WebSocket URL at the serverless function. Works slowly and very unstably. See README_RU.md in the `one-adapter` branch for details.

2. Adapter + helper variant (branch `main`) — one component on the server side, one on the client side. Proxies arbitrary TCP connections and works much more stably and faster. **(Use this version if you don't know from where to start!)**

3. Experimental variant (branch `experimental`), similar to variant 2 but instead of a custom multiplexing/error-correction algorithm it uses QUIC wrapped in WebSocket messages.

4. Experimental IP-tunnel variant (branch `ip-tun`): instead of proxying TCP streams, the MAUI client opens a TUN device on Android and tunnels raw IP packets to the adapter (**Android-only**). Speeds are roughly on par with a 2G mobile connection — bad for web browsing, but acceptable for Telegram. See README/README_RU on the `ip-tun` branch for details.

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


## Important notices

> HIGHLY RECOMMENDED: BEFORE YOU UPLOAD THE CLOUD FUNCTION TO YANDEX, OBFUSCATE THE JAVASCRIPT CODE (for example with `javascript-obfuscator`). The plain source may attract unwanted attention. It is fine to deploy the plain code once to verify the setup works end-to-end — but as soon as you confirm it works, replace it with an obfuscated build.

> This is a proof-of-concept and a messy hobby project. No guarantees of any kind. The wire protocol, configuration format and APIs can change at any time - sometimes every day. Whenever you pull a new revision, ALWAYS update all three components together: Cloud Function (`bridge-cloud/`), adapter, and helper / MAUI app. Mixing versions across these components will almost certainly break the tunnel in subtle and frustrating ways.

## Installation overview

Three pieces need to be in place. Detailed configuration for each lives in its own section below; here is the high-level order:

1. **Adapter** — build it (see [Adapter / Build](#build)) and run it on a remote server, ideally next to whatever you ultimately proxy through (Dante / XRay-core / etc.). It must expose its HTTP recovery endpoint to the public internet so the Serverless Function can reach it on cold start (default path `/conn-ids`, configurable via `http.path` — see [Customizing endpoint paths](#customizing-endpoint-paths)).
2. **Cloud Function** — deploy [`bridge-cloud/`](bridge-cloud/) to Yandex Cloud Functions and bind it to an API Gateway. Set the `HTTP_URL` env var to the **full** URL of the adapter's HTTP recovery endpoint (e.g. `https://<server>:<port>/conn-ids`, or whatever path you set in the adapter's `http.path`), and use the same `AUTH_TOKEN` shared secret on all three components. Don't forget to obfuscate the JS before uploading (see notice above).
3. **Client** — configure the Go helper or the MAUI app with the same `bridge.url` (the API Gateway URL ending in `/_helper`) and `authToken`. Start it, point your apps at the helper's local listen port, and you're done.

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

### Relay mode

If the helper cannot reach the `wsSend` gRPC endpoint (the YC API — for example on a restricted network), set `wsApi.relay: true`. The helper sends data through its upstream WebSocket and the Serverless Function relays it to the adapter. The reverse path (adapter → helper) still uses `wsSend` directly. Relay mode is slower and less stable for the same reasons as the single-adapter (one-branch) variant, but is still more stable thanks to proper multiplexing.

> **Recommendation:** don't use relay mode. Turn it on only if it doesn't work without it (i.e. your network blocks the access to gRPC API endpoint). Direct mode is faster and more stable in every scenario.

---

## One more time, important things

How long until Yandex bans you for this — no idea.

Recommendations:

- Use this only for proxying the most important low-traffic services (for example, tunnel through to a SOCKS proxy and plug it into Telegram as 127.0.0.1). Don't abuse it by pushing large amounts of data.

- Before uploading the serverless function code, run it through any JavaScript obfuscator (a couple of times, even). Also, rename the default endpoint paths (`/_adapter`, `/_helper`, `/conn-ids`) to random ones of your own — see [Customizing endpoint paths](#customizing-endpoint-paths) below for how to do that.

### Also important

If you use a proxy client that works as a TUN, you must add an exclusion for the helper process, otherwise you'll get an infinite loop and nothing will work. The mobile app (MAUI) writes endpoint domains and IP addresses to the log on connect — you can add those to exclusions if per-process exclusions aren't available. That said, in the Happ client it didn't work for me even with that (but it works fine without TUN, e.g. for the same TG).

### Recommended setup for Android (proven stable)

The combination below has proven both effective and stable:

- On the phone: install the MAUI client (this app, BTF) and [v2rayNG](https://github.com/2dust/v2rayNG). In v2rayNG, enable per-app proxying and pick the apps you actually want to route through the tunnel (e.g. Chrome and Telegram only — this is important so v2rayNG does NOT try to route BTF's own upstream traffic, which would cause a loop). Create a new outbound profile of type SOCKS (or VLESS) and point it to `127.123.45.67:5080`. Start BTF first and connect to the server, then enable v2rayNG.
- On the adapter side (VPS): run Dante (SOCKS) or XRay (VLESS) listening on whatever address the adapter forwards to (i.e. the adapter's `target.address`). The adapter delivers each incoming TCP stream to it and the proxy then exits to the open internet.

Flow: `app → v2rayNG (per-app) → BTF helper :5080 → YC → BTF adapter → Dante/XRay → internet`.

---

## Customizing endpoint paths

To avoid a recognisable URL structure, you can rename all of the default endpoint paths to anything you like. None of these paths are part of the wire protocol — they are just labels. Defenders fingerprint on whatever is unique, so changing them is recommended.

**WebSocket paths (`/_adapter`, `/_helper`) — configurable without touching code.**

1. In [`bridge-cloud/spec.yaml`](bridge-cloud/spec.yaml), rename the two top-level path keys (`/_adapter` and `/_helper`) to arbitrary strings, e.g. `/q7x` and `/k2m`. **Do not** change the `context.route` values (`adapter`, `helper`) — those are internal labels the function code switches on.
2. Update `bridge.url` in [`adapter.config.yaml`](adapter-and-helper/adapter.config.yaml) to use the new adapter path.
3. Update `bridge.url` in [`helper.config.yaml`](adapter-and-helper/helper.config.yaml) (or the **Bridge URL** field in the MAUI app) to use the new helper path.
4. Redeploy the API Gateway with the updated spec.

**HTTP path on the adapter (default `/conn-ids`) — configurable via `http.path`.**

The adapter's HTTP endpoint that the Cloud Function polls on cold start defaults to `/conn-ids`. To rename it:

1. Set `http.path` in [`adapter.config.yaml`](adapter-and-helper/adapter.config.yaml) to your random path, e.g. `/p4f9z2`. Restart the adapter.
2. Set the Cloud Function's `HTTP_URL` env var to the **full** URL including the new path, e.g. `https://your-server:3001/p4f9z2`. Redeploy the function (or update the env var on the existing version).

No source edits are needed — both ends are config-driven.

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
  path: "/conn-ids"

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
| `http.listenPort` | HTTP port for the recovery endpoint. **Required** (must be > 0) — the Cloud Function polls it on cold start; without it the tunnel breaks as soon as a function instance recycles. |
| `http.path` | URL path of the recovery endpoint. Default `/conn-ids`. Change it to something random of your own — see [Customizing endpoint paths](#customizing-endpoint-paths). The same full URL (host + path) must be set in the Cloud Function's `HTTP_URL` env var. |
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
| `http.path` (default `/conn-ids`) | GET | Returns `{"adapterConnId":"...","helperConnId":"..."}`. Authorization: `Bearer <authToken>`. |

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

#### Obfuscate `index.js` first (strongly recommended)

Before packaging, run `index.js` through a JavaScript obfuscator. The point is to make it harder for Yandex Cloud itself to spot the function as a censorship-bypass bridge and ban it — the plain source has very recognisable identifiers (`adapterConnId`, `helperConnId`, the route switch, etc.) and would light up any automated scan of uploaded function code.

Using [`javascript-obfuscator`](https://github.com/javascript-obfuscator/javascript-obfuscator):

```bash
npm install -g javascript-obfuscator

cd bridge-cloud
cp index.js index.original.js   # keep a clean copy outside the zip
javascript-obfuscator index.original.js \
  --output index.js \
  --compact true \
  --control-flow-flattening true \
  --control-flow-flattening-threshold 0.75 \
  --dead-code-injection true \
  --dead-code-injection-threshold 0.4 \
  --string-array true \
  --string-array-encoding base64 \
  --string-array-threshold 0.8 \
  --identifier-names-generator hexadecimal \
  --rename-globals false \
  --self-defending true \
  --target node
```

Windows (PowerShell) — the same flags, just line-continued differently:

```powershell
npm install -g javascript-obfuscator

cd bridge-cloud
Copy-Item index.js index.original.js
javascript-obfuscator index.original.js `
  --output index.js `
  --compact true `
  --control-flow-flattening true `
  --control-flow-flattening-threshold 0.75 `
  --dead-code-injection true `
  --dead-code-injection-threshold 0.4 `
  --string-array true `
  --string-array-encoding base64 `
  --string-array-threshold 0.8 `
  --identifier-names-generator hexadecimal `
  --rename-globals false `
  --self-defending true `
  --target node
```

Notes:

- Keep `--rename-globals false` and `--target node` — the YC runtime calls `exports.handler`, so the exported entrypoint name must stay intact.
- `--self-defending` makes the obfuscated code brittle if reformatted; do **not** re-edit `index.js` after obfuscation. Edit `index.original.js` and re-run the obfuscator.
- Running the obfuscator twice (output of pass 1 as input of pass 2) is fine and a bit more thorough, but slows cold start.

#### Package and deploy

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
  --environment "AUTH_TOKEN=<your-shared-secret>,HTTP_URL=<adapter-http-recovery-url>"
```

| Variable | Required | Description |
|------------|-------------|----------|
| `AUTH_TOKEN` | Yes | Shared secret (same value as `bridge.authToken`) |
| `HTTP_URL` | Yes | Full URL of the adapter's HTTP recovery endpoint, including the path (e.g. `https://your-server:3001/conn-ids`, or whatever you set for `http.path` in the adapter config). Used to fetch connection IDs on cold start; needed to restore state across multiple instances. |

### 3. Create the API Gateway

Edit `bridge-cloud/spec.yaml` — replace the two placeholders:

- `${FUNCTION_ID}` — the function ID from step 2
- `${SERVICE_ACCOUNT_ID}` — the service account ID from step 1

You can do this in one shot with `sed` (Linux/macOS), assuming `$FUNCTION_ID` and `$SA_ID` are exported from the previous steps:

```bash
sed -i.bak \
  -e "s|\${FUNCTION_ID}|$FUNCTION_ID|g" \
  -e "s|\${SERVICE_ACCOUNT_ID}|$SA_ID|g" \
  spec.yaml
```

On Windows (PowerShell):

```powershell
(Get-Content spec.yaml -Raw) `
  -replace '\$\{FUNCTION_ID\}',        $env:FUNCTION_ID `
  -replace '\$\{SERVICE_ACCOUNT_ID\}', $env:SA_ID `
  | Set-Content spec.yaml -NoNewline
```

While you're here, this is also a good moment to rename the WebSocket path keys from the defaults (`/_adapter`, `/_helper`) to something non-fingerprintable — see [Customizing endpoint paths](#customizing-endpoint-paths). For example, to rename them to `/q7x` and `/k2m`:

```bash
sed -i.bak \
  -e 's|^  /_adapter:|  /q7x:|' \
  -e 's|^  /_helper:|  /k2m:|' \
  spec.yaml
```

PowerShell equivalent:

```powershell
(Get-Content spec.yaml) `
  -replace '^(\s*)/_adapter:', '$1/q7x:' `
  -replace '^(\s*)/_helper:',  '$1/k2m:' `
  | Set-Content spec.yaml
```

If you go this route, update `bridge.url` in both `adapter.config.yaml` and `helper.config.yaml` to use the new paths.

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