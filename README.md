# YC Serverless Functions IP-tunnel

(unofficial alias: **Bridge to Freedom**)

A point-to-point **IP-level VPN** over a YC API Gateway WebSocket (Serverless Function for discovery only) — designed to bypass Russian internet censorship under strict whitelist filtering.

> ⚠️ **This implementation lives on the `ip-tun` branch.** Make sure you have it checked out: `git checkout ip-tun`. The `main` branch and the other historical branches use different (and incompatible) wire protocols.

See [README_RU.md](README_RU.md) for documentation in Russian.

## TL;DR — what's actually usable

- **Not a general-purpose VPN.** Throughput in practice is somewhere around an old 2G/EDGE mobile connection. Loading modern websites is painful.
- **Surprisingly good for Telegram.** The protocol is chatty but tolerates high latency well; messaging and small media work usably.
- **Recommended setup**: install the Android app, enable **per-app tunneling**, and route **only Telegram** (and maybe one or two similar low-bandwidth, latency-tolerant apps) through it. Everything else stays on your normal connection.

Two Go binaries — **adapter** (on the target side, typically a VPS in RU close to Yandex) and **helper** (on the client device) — each open a TUN interface and maintain an upstream WebSocket to the YC API Gateway. Reordering, retransmission, congestion control are entirely handled by the host OS TCP/IP stack — there is no per-stream framing or sequence numbers in the wire protocol. There is also a **MAUI Android app** that does the same thing as the Go helper but using Android's `VpnService` to obtain the TUN, with built-in **per-app tunneling** (route only selected apps).

In the IP-tunnel variant on this branch:

Two Go binaries — **adapter** (on the target side, typically a VPS in RU close to Yandex) and **helper** (on the client device) — each open a TUN interface and maintain an upstream WebSocket to the YC API Gateway. In "no relay" mode, IP packets travel **directly** through the YC WebSocket management API (gRPC `wsSend`), bypassing the Serverless Function on the data path. The Serverless Function is only used so the helper and adapter can find each other.

```
                                       Yandex Cloud
                          ┌─────────────────────────────────┐
[client apps] ──► TUN ──► Helper ──wsSend(gRPC)──► API GW ──WS──► Adapter ──► TUN ──► (kernel routes / NATs onward)
                          ▲                       │           ▲
[client apps] ◄── TUN ◄── Helper ◄──WS── API GW ◄─wsSend(gRPC)─ Adapter ◄── TUN ◄── (return packets from internet)
                                          │
                                  Cloud Function
                                  (discovery only)
```

## How it works

1. **Adapter** dials the API Gateway at `/_adapter`, authenticates with the Cloud Function (HELLO handshake), and is assigned its WebSocket connection ID. It also opens a local TUN device with a configured `/30` endpoint (e.g. `10.200.0.1`).
2. **Helper** dials `/_helper`, authenticates, learns the adapter's connection ID, and opens its own TUN with the matching peer endpoint (e.g. `10.200.0.2`). The Cloud Function notifies the adapter of the helper's ID.
3. Each side reads raw IP packets from its TUN device, wraps each packet in a `PACKET` (or batched `PACKET_BATCH`) frame, and ships it to the peer via `wsSend` (or via the Cloud Function relay in relay mode).
4. The receiving side decodes the frame and writes the packet to its own TUN. The host kernel's TCP/IP stack then handles everything else — no per-stream state, no reordering, no FIN/RST bookkeeping at the bridge layer.

Because the OS TCP stack on each side performs reordering and retransmission natively, dropped or out-of-order frames are now a non-issue: TCP will detect the loss, retransmit, and congestion-control as usual. (For UDP traffic, packet loss simply manifests as packet loss — that's already what UDP applications expect.)

### Wire protocol (very small)

`[1B type][payload...]`

Types:

| Type | Hex  | Direction | Payload |
|---|---|---|---|
| HELLO        | `0x01` | →     | `[1B version][token]` |
| HELLO_OK     | `0x02` | ←     | `[2B][ownId][2B][peerId][2B][iamToken]` |
| HELLO_ERR    | `0x03` | ←     | reason |
| PEER_CONN    | `0x04` | ↔     | `[2B][peerId][2B][iamToken]` |
| PEER_GONE    | `0x05` | ↔     | — |
| SYNC         | `0x06` | →     | — (request peer rediscovery) |
| PING / PONG  | `0xF0` / `0xF1` | ↔ | (PONG carries refreshed iamToken) |
| **PACKET**   | `0x10` | ↔     | one raw IP packet |
| **PACKET_BATCH** | `0x11` | ↔ | repeated `[2B len][raw IP packet]` |

There is no `streamID`, no `seqID`, no `OPEN`/`OPEN_OK`/`FIN`/`RST` — the v4 framing is gone. A datagram lost in flight is just a dropped IP packet, and the kernel handles it.

### Write coalescing

`writeCoalescing` packs IP packets that the TUN delivers within a short window (default `delayMs`) into a single `PACKET_BATCH` frame. This drops the per-packet WebSocket overhead for chatty workloads (many small TCP segments / ACKs) at the cost of a tiny added latency. Default is **off** because TCP's own coalescing usually does enough; turn it on if you're shipping a lot of small packets and `wsSend` rate is the bottleneck.

### Relay mode

If the helper cannot reach the `wsSend` gRPC endpoint (the YC API), set `wsApi.relay: true`. The helper sends data through its upstream WebSocket and the Serverless Function relays it to the adapter. The reverse path (adapter → helper) still uses `wsSend` directly. Relay mode is slower and less stable for the same reasons as the single-adapter variant, but is still usable.

**Important**: one adapter — one function — one helper. Multiple helpers against the same adapter will produce undefined behavior.

---

## Important caveats

How long until Yandex bans you for this — no idea.

Recommendations:

- Use this only for proxying the most important low-traffic services. Don't push large amounts of data.
- Before uploading the Cloud Function code, run it through a JS obfuscator a couple of times. Replace all the default paths (`/_upstream`, `/_helper`, `/_conn-ids`) with random values of your own.

### Real-world performance

This is a serverless WebSocket pretending to be a layer-3 tunnel. Don't expect miracles:

- **Web browsing** — barely usable. Page loads are slow enough to feel like an old 2G/EDGE connection: think a few hundred kbps at best, with multi-second TLS handshakes on big sites. Streaming and large downloads — forget it.
- **Telegram** — works **surprisingly well**. The Telegram protocol is small-message, latency-tolerant and reconnect-friendly, which happens to match exactly what this tunnel is good at. Chat, voice messages, small images / stickers all work usably.
- **Recommended setup**: use the Android app's **per-app tunneling** to route **only Telegram** (and maybe one or two similar latency-tolerant low-bandwidth apps) through the tunnel. Leave everything else on your regular connection.

### Loop avoidance

The helper needs an outbound network path to the YC API Gateway that does **not** itself go through the tunnel — otherwise the WebSocket would be forwarded through itself and nothing would work.

- The **Go helper** runs as a normal process and (on Linux/macOS) you must add a host route for the API Gateway IP via your real default gateway, e.g. `ip route add <gw-ip>/32 via <real-gateway>`. Logs print the resolved IPs on startup.
- The **MAUI Android app** uses `VpnService.Builder.addDisallowedApplication(packageName)` to exclude itself from its own VPN automatically — no manual routing needed.
- On Windows, set up a per-host route via your real interface (`route add <gw-ip> <real-gateway>`).

---

## Adapter

Goes on a VPS. Ideally in Russia, close to Yandex.

### Build

Requires Go 1.21+ (toolchain will auto-upgrade to the version required by `wireguard/tun`).

```bash
cd adapter-and-helper
go build -o adapter ./cmd/adapter
```

On **Windows**, copy `wintun.dll` (matching your CPU architecture) next to the binary. Get it from <https://www.wintun.net/>. WireGuard ships it the same way.

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

tun:
  name: "btf0"
  address: "10.200.0.1"
  peerAddress: "10.200.0.2"
  mtu: 1400

http:
  listenPort: 3001

writeCoalescing:
  enabled: false
  delayMs: 5

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
| `tun.name` | OS interface name. Linux: `btf0`. Windows: wintun adapter display name. macOS: leave empty (utun assigns one). |
| `tun.address` | Local tunnel IP, e.g. `10.200.0.1`. Must form a `/30` with `peerAddress`. |
| `tun.peerAddress` | Remote tunnel IP, e.g. `10.200.0.2`. |
| `tun.mtu` | TUN MTU. 1400 is a safe default — leaves room for the WebSocket + TLS + outer IP overhead. |
| `http.listenPort` | HTTP port for the `/conn-ids` endpoint (used by the Cloud Function on cold start) |
| `writeCoalescing.enabled` | Pack multiple IP packets into a single `PACKET_BATCH` frame. Reduces `wsSend` rate for chatty traffic. |
| `writeCoalescing.delayMs` | Batching window (ms). Forced flush at 32 KiB. Recommended 1–10 ms if enabled. |
| `wsApi.mode` | `grpc` (the only option) |

### Server-side IP forwarding (Linux)

The adapter only emits packets onto its TUN — it does **not** NAT or forward them. To let helper-side traffic actually reach the internet, configure the Linux host:

```bash
# Enable IPv4 forwarding
sudo sysctl -w net.ipv4.ip_forward=1

# Masquerade the helper's traffic as the server's egress IP. Replace eth0 with
# your actual upstream interface and 10.200.0.0/30 with your tun /30.
sudo iptables -t nat -A POSTROUTING -s 10.200.0.0/30 -o eth0 -j MASQUERADE
sudo iptables -A FORWARD -i btf0 -o eth0 -j ACCEPT
sudo iptables -A FORWARD -i eth0 -o btf0 -m state --state RELATED,ESTABLISHED -j ACCEPT
```

Persist these with `iptables-persistent` / `nftables` / your distro's preferred tool.

If you only want to expose specific services, leave forwarding off and bind those services directly to the tunnel IP `10.200.0.1`.

### Run

```bash
sudo ./adapter adapter.config.yaml
```

Root (or `CAP_NET_ADMIN`) is required to create the TUN device.

### HTTP endpoints

| Endpoint | Method | Description |
|-----------|-------|----------|
| `/conn-ids` | GET | Returns `{"adapterConnId":"...","helperConnId":"..."}`. Authorization: `Bearer <authToken>`. |

---

## Helper (Go)

Goes on the client device. There is also a MAUI **Android** app — see [maui-client/README.md](maui-client/README.md).

### Build

```bash
cd adapter-and-helper
go build -o helper ./cmd/helper
```

On Windows, drop `wintun.dll` next to the binary.

### Configuration

```yaml
bridge:
  url: "wss://<api-gateway-domain>/_helper"
  authToken: "<shared-secret>"
  reconnect:
    initialDelayMs: 1000
    maxDelayMs: 30000
    backoffMultiplier: 2
  pingIntervalMs: 30000

tun:
  name: "btf0"
  address: "10.200.0.2"
  peerAddress: "10.200.0.1"
  mtu: 1400

writeCoalescing:
  enabled: false
  delayMs: 5

wsApi:
  mode: "grpc"
  relay: false

logging:
  level: "info"
```

| Key | Description |
|------|----------|
| `tun.address` / `tun.peerAddress` | Mirror of the adapter's TUN config (helper's local = adapter's peer and vice versa). |
| `wsApi.relay` | `true` = data through upstream WS (Cloud Function relays). `false` = direct `wsSend` to the adapter. |

### Run

```bash
sudo ./helper helper.config.yaml
```

### Routing on the client

The helper opens the TUN but does **not** install a default route. Decide what you want to send through the tunnel and add a route yourself. Two common shapes:

**A. Route everything through the tunnel** (full VPN). Pin the API Gateway IP to your real gateway first to avoid loops:

```bash
GW_IP=$(getent hosts <api-gateway-domain> | awk '{print $1}' | head -n1)
REAL_GW=$(ip route | awk '/default/ {print $3; exit}')
REAL_DEV=$(ip route | awk '/default/ {print $5; exit}')

# Loop-avoidance: API Gateway via the real network, never via the tunnel
sudo ip route add ${GW_IP}/32 via ${REAL_GW} dev ${REAL_DEV}

# Default via the tunnel
sudo ip route add 0.0.0.0/1 via 10.200.0.1 dev btf0
sudo ip route add 128.0.0.0/1 via 10.200.0.1 dev btf0
```

**B. Route only specific subnets** (e.g. one application's targets):

```bash
sudo ip route add 203.0.113.0/24 via 10.200.0.1 dev btf0
```

DNS is not handled here — point your resolver at something reachable through the tunnel (or set `/etc/resolv.conf` to a public resolver and let it route over the tunnel).

---

## Deploying the Serverless Function (YC)

The cloud function **is** protocol-aware: it parses the HELLO frame, validates the auth token, exchanges connection IDs, and (in relay mode) forwards `PACKET`/`PACKET_BATCH` frames between the two peers. When you upgrade the Go binaries to the IP-tunnel protocol you must redeploy `bridge-cloud/index.js` together with them, otherwise the handshake will fail (the old v4 function expects a 9-byte stream-frame header that the v5 binaries no longer emit).

### Prerequisites

- A YC account with billing enabled
- `yc` CLI configured (`yc init`)
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

### 2. Create and deploy the Cloud Function

```bash
cd bridge-cloud
zip -r bridge-function.zip index.js package.json

yc serverless function create --name bridge-fn
FUNCTION_ID=$(yc serverless function get bridge-fn --format json | jq -r .id)

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
| `ADAPTER_URL` | Yes | HTTP(S) URL of the adapter (`https://your-server:3001`). Used to fetch `/conn-ids` on cold start. |

### 3. Create the API Gateway

Edit `bridge-cloud/spec.yaml` — replace `${FUNCTION_ID}` and `${SERVICE_ACCOUNT_ID}`. Then:

```bash
yc serverless api-gateway create --name bridge-gw --spec spec.yaml
GW_DOMAIN=$(yc serverless api-gateway get bridge-gw --format json | jq -r .domain)
echo "Gateway: wss://$GW_DOMAIN"
```

### 4. Run the adapter and helper

Set `bridge.url` accordingly in each config and run as described above.

### 5. Send traffic

There is **no client port to connect to** — clients use the TUN by routing traffic at the IP layer. Add routes (see "Routing on the client") and just use the network normally.

To verify the tunnel is up:

```bash
ping 10.200.0.1   # from the helper side, pings the adapter's tunnel IP
```

### 6. Logs

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

Set `pingIntervalMs` below 10 minutes to avoid idle disconnects. The 128 KB message limit is the cap on a single `PACKET_BATCH` payload — well above any IP packet size.

## License

WTFPL, see [LICENSE](LICENSE).
It's just a PoC. There will be no further development, support, or answers to questions.
