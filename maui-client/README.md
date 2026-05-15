# Bridge to Freedom — Client app

Cross-platform GUI client for the Bridge to Freedom TCP tunnel. Acts as the **helper** — listens on a local TCP port and tunnels connections to the adapter through YC.

## What's new — 2026-05-15

This release is heavily optimised. The helper now reorders out-of-order frames on receive, pre-registers streams before sending `OPEN` (so DATA packets that arrive immediately after `OPEN_OK` are never dropped), uses an async per-stream write queue so a slow local consumer no longer stalls every other stream, and does a graceful half-close on `FIN` so HTTP responses are no longer truncated. In practice: connections come up noticeably faster and the tunnel is significantly more stable, especially on mobile.

## Supported platforms

- **Android** (API 26+ / Android 8.0+)
- **iOS** (15.0+)
- **Windows** (10 1809+)
- **macOS** (via Mac Catalyst, macOS 12+)
- **Linux** (via the GTK4 backend)

> **Note on Linux:** A Linux build target is not set up in this repository, but MAUI now officially supports Linux through the GTK4 backend. Follow Microsoft's guide: <https://learn.microsoft.com/en-us/dotnet/maui/developer-tools/platform-backends/linux-gtk4?view=net-maui-10.0>. As an alternative, you can use the Go `helper` binary (`adapter-and-helper/cmd/helper`).

## Requirements

- .NET 10 SDK
- MAUI workload: `dotnet workload install maui`
- For Android: Android SDK (installed automatically with the MAUI workload)
- For iOS/macOS: a Mac with Xcode

## Important

If you use a proxy client that works as a TUN, you must add an exclusion for the helper process, otherwise you'll get an infinite loop and nothing will work. On connect, the app logs endpoint domains and IP addresses — you can add those to exclusions if per-process exclusions aren't available. That said, in the Happ client it didn't work for me even with that (but it works fine without TUN, e.g. for Telegram).

## Build

```bash
cd client

# Android (Debug)
dotnet build -f net10.0-android

# Android (Release APK)
dotnet publish -f net10.0-android -c Release

# iOS (requires a Mac with Xcode)
dotnet build -f net10.0-ios

# Windows
dotnet build -f net10.0-windows10.0.19041.0

# macOS (Mac only)
dotnet build -f net10.0-maccatalyst
```

The Android APK will be in `bin/Release/net10.0-android/publish/`.

## Usage

1. Enter **Bridge URL** — the helper endpoint of the API Gateway (e.g. `wss://gateway.example.com/_helper`)
2. Enter **Auth Token** — the shared secret matching the adapter and Cloud Function
3. Set **Listen Address** and **Port** — where clients should connect (default `127.0.0.1:5080`)
4. If your device cannot reach the `wsSend` API directly, enable **Relay mode**. Recommendation: don't enable relay mode unless you have to — only use it if direct mode doesn't work for you. Direct mode is faster and more stable.
5. Press **CONNECT**

The app keeps the tunnel running in the background:
- **Android**: foreground service with a wake lock
- **iOS**: `beginBackgroundTask` + `BGProcessingTask`

Press **DISCONNECT** to stop.

Settings are saved automatically and restored on next launch; they can also be imported/exported as a URL.

## Recommended setup for Android (proven stable)

The combination below has proven both effective and stable:

- On the phone: install this app (BTF) and [v2rayNG](https://github.com/2dust/v2rayNG). In v2rayNG, enable per-app proxying and pick the apps you actually want to route through the tunnel (e.g. Chrome and Telegram only — important so v2rayNG does NOT try to route BTF's own upstream traffic, which would cause a loop). Create a new outbound profile of type SOCKS (or VLESS) and point it to `127.0.0.1:5080`. Start BTF first and connect to the server, then enable v2rayNG.
- On the adapter side (VPS): run Dante (SOCKS) or XRay (VLESS) listening on whatever address the adapter forwards to (i.e. the adapter's `target.address`). The adapter delivers each incoming TCP stream to it and the proxy then exits to the open internet.

Flow: `app → v2rayNG (per-app) → BTF helper :5080 → YC → BTF adapter → Dante/XRay → internet`.
