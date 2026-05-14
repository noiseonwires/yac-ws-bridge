# Bridge to Freedom — Client app

Cross-platform GUI client for the Bridge to Freedom TCP tunnel. Acts as the **helper** — listens on a local TCP port and tunnels connections to the adapter through YC.

> **See also:** an alternative **IP-tunnel** build of this client lives on the [`ip-tun`](../../tree/ip-tun) branch. It uses an Android `VpnService` instead of a SOCKS-style local listener and supports **per-app tunneling**. Android-only; speed is roughly 2G-mobile, so it is bad for web browsing but works surprisingly well for Telegram.

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
3. Set **Listen Address** and **Port** — where clients should connect (default `127.0.0.1:1080`)
4. If your device cannot reach the `wsSend` API directly, enable **Relay mode**
5. Press **CONNECT**

The app keeps the tunnel running in the background:
- **Android**: foreground service with a wake lock
- **iOS**: `beginBackgroundTask` + `BGProcessingTask`

Press **DISCONNECT** to stop.

Settings are saved automatically and restored on next launch; they can also be imported/exported as a URL.
