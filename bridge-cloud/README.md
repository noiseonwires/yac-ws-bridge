# Bridge to Freedom — Yandex Cloud Deployment

## Architecture

```
Client ──WS──► YC API Gateway ──invoke──► Cloud Function ──WS API──► API Gateway ──WS──► Adapter
                   │                            │
                   │ manages WS connections      │ global vars = state
                   │                            │ (upstream connId + client maps)
                   └────────────────────────────┘
```

The Cloud Function is invoked per WebSocket event. It uses:
- **Global variables** to store upstream connectionId and client mappings (survive between warm invocations)
- **API Gateway WebSocket management API** to send messages to other connections
- **POST fallback** to wake up the adapter if no upstream WS is connected

No database needed. On cold start, state is lost — adapter and clients reconnect automatically.

## Prerequisites

1. Yandex Cloud account with billing enabled
2. `yc` CLI installed and configured
3. A folder in your cloud for the project

## Step-by-step Setup

### 1. Create a Service Account

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

### 2. Create the Cloud Function

Package the function:
```bash
cd bridge-cloud
zip -r bridge-function.zip index.js package.json
```

Create the function:
```bash
yc serverless function create --name bridge-fn
FUNCTION_ID=$(yc serverless function get bridge-fn --format json | jq -r .id)
```

Create a version:
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
  --environment "AUTH_TOKEN=<your-shared-secret>,ADAPTER_URL=<adapter-wakeup-url>"
```

### 3. Create the API Gateway

Edit `spec.yaml` — replace `${FUNCTION_ID}` and `${SERVICE_ACCOUNT_ID}` with actual values.

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

### 4. Configure the Adapter

Update `adapter.config.yaml`:
```yaml
bridge:
  url: "wss://<api-gateway-domain>/_upstream"
  authToken: "<same-shared-secret>"
  reconnect:
    initialDelayMs: 1000
    maxDelayMs: 30000
    backoffMultiplier: 2
  pingIntervalMs: 30000

wakeup:
  listenPort: 3001

target:
  url: "ws://127.0.0.1:9090"

logging:
  level: "info"
```

### 5. Point Clients to the API Gateway

Clients connect to:
```
wss://<api-gateway-domain>/chatw
```

## How It Works

| Event | What happens |
|-------|-------------|
| Adapter CONNECT to `/_upstream` | Function stores connectionId in global `upstreamConnId` |
| Client CONNECT to `/chatw` | Function hashes connectionId→clientId, stores in global Map, sends `CLIENT_CONNECTED` to adapter via WS API |
| Client MESSAGE | Function encodes as `DATA_C2T`, sends to adapter via WS API |
| Adapter MESSAGE (`DATA_T2C`) | Function looks up client connectionId from global Map, sends to client via WS API |
| Client DISCONNECT | Function sends `CLIENT_DISCONNECTED` to adapter, removes from Map |
| Adapter DISCONNECT | Function clears `upstreamConnId` |
| Cold start | State lost — adapter reconnects (or wakeup POST triggers it), clients reconnect naturally |

## Timeouts & Limits

| Limit | Value |
|-------|-------|
| Max WS connection lifetime | 60 minutes |
| Idle timeout (no messages) | 10 minutes |
| Max message size | 128 KB |
| Max frame size | 32 KB |
| Function execution timeout | 10 seconds (configurable) |

Send periodic pings from the adapter to keep the connection alive.

## Cost

- **Cloud Functions**: pay per invocation + execution time
- **API Gateway**: pay per requests + WebSocket connections
- WebSocket send messages via management API: **free**

All pay-per-use with generous free tiers. No database costs.
