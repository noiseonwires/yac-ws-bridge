# Bridge to Freedom — Android client

GUI client for the Bridge to Freedom IP tunnel. Acts as the **helper** end of the tunnel: opens a system VPN (`VpnService`) and ships raw IP packets through YC API Gateway to the adapter on the other side.

> ⚠️ **This client lives on the `ip-tun` branch** of the repo. `git checkout ip-tun` before building. Other branches contain different (incompatible) versions of the helper.

See [README_RU.md](README_RU.md) for documentation in Russian.

## What it's good for (and what it isn't)

This is a serverless WebSocket pretending to be a layer-3 tunnel. Real-world speed feels like an old 2G/EDGE mobile connection — perfectly **unsuitable** for general web browsing, streaming or large downloads.

It is, however, **surprisingly good for Telegram**. The Telegram protocol is small-message, latency-tolerant and reconnect-friendly, which matches exactly what this tunnel can deliver. Chats, voice messages and small media work well.

**Recommended setup**: enable **per-app tunneling** in the app and route **only Telegram** (optionally one or two similar low-bandwidth, latency-tolerant apps) through the VPN. Leave everything else on your normal connection — this gives you fast unrestricted browsing for normal apps and a working Telegram on top.

## Supported platforms

- **Android only** (API 26+ / Android 8.0+) — with built-in **per-app tunneling**.

The IP-tunnel design requires direct, unmediated access to a TUN device, which is not possible to add as pure managed-MAUI code on other platforms:

- **iOS** needs a separate `NEPacketTunnelProvider` system extension target — out of scope for a single MAUI project.
- **Windows** needs a `wintun.dll` native loader — use the Go [`helper`](../adapter-and-helper/cmd/helper) binary instead.
- **macOS** needs a system extension with the Network Extension entitlement — same story, use the Go helper.

If you need a desktop client, build the Go [`helper`](../adapter-and-helper) and follow the routing instructions in the root [README.md](../README.md#routing-on-the-client).

## Requirements

- .NET 10 SDK (preview at the time of writing)
- MAUI workload: `dotnet workload install maui`
- Android SDK (installed automatically with the MAUI workload)

## Permissions

The app requests the following at runtime / install time:

- `BIND_VPN_SERVICE` — required to register `BtfVpnService` as a system VPN.
- `FOREGROUND_SERVICE` / `FOREGROUND_SERVICE_DATA_SYNC` — required to keep the VPN alive in the background.
- `WAKE_LOCK` — held while the tunnel is up so the device doesn't sleep mid-session.
- `INTERNET` — for the upstream WebSocket.

On the first connect, Android shows the system "Connection request" dialog ("…wants to set up a VPN connection"). Approve it once.

## Build

```bash
cd maui-client

# Debug
dotnet build -f net10.0-android

# Release APK
dotnet publish -f net10.0-android -c Release
```

The APK lands in `bin/Release/net10.0-android/publish/`.

## Usage

1. Enter **Bridge URL** — helper endpoint of the API Gateway (e.g. `wss://gateway.example.com/_helper`).
2. Enter **Auth Token** — the shared secret that matches the adapter and Cloud Function.
3. **Tunnel Address** — local IP for the on-device TUN (default `10.200.0.2`).
4. **Peer Address** — adapter's tunnel IP (default `10.200.0.1`).
5. **MTU** — default `1400`. Lower if your network drops larger packets.
6. **Relay mode** — enable if the device cannot reach the YC `wsSend` gRPC API directly.
7. **Per-app tunneling** *(strongly recommended — see "What it's good for" above)* — pick a list of apps that should be routed through the tunnel; everything else stays on the regular connection. For the recommended Telegram-only setup, select just Telegram here. If you leave the list empty, **all** apps are routed through the tunnel (except this app itself, automatically).
8. Press **CONNECT** and approve the VPN permission prompt.

The app routes the device's default route (`0.0.0.0/0`) through the TUN with DNS `1.1.1.1` / `8.8.8.8`. If per-app tunneling is configured, Android applies the allow-list at the OS level — non-selected apps bypass the VPN entirely. Either way, this app's own package is excluded from its own VPN so the WebSocket itself doesn't loop through.

The foreground service shows a persistent notification while the tunnel is up. Press **DISCONNECT** to stop and tear down the VPN.

Settings are saved automatically and restored on next launch.

## Loop avoidance

The Android `VpnService` API does not allow per-host route exclusions, so the app excludes its own package instead. If you also use another VPN/TUN-style proxy app on the device, only one of them can be active at a time — Android only allows a single active VPN.

## Troubleshooting

- **"VPN permission denied"** — the user rejected the system prompt. Toggle CONNECT again to re-prompt.
- **Tunnel up but no traffic** — most often the adapter side hasn't enabled `net.ipv4.ip_forward` or hasn't installed the MASQUERADE rule. See the root [README.md](../README.md#server-side-ip-forwarding-linux). If you're using per-app tunneling, double-check that the apps you expect to be tunneled are actually selected in the list.
- **Disconnects every ~10 minutes** — `pingIntervalMs` on the adapter side is too high; YC closes idle WebSockets at 10 min.
- **Telegram works, browser doesn't** — that's expected. See "What it's good for" at the top.
