# Bridge to Freedom — Client app

Cross-platform GUI client for the Bridge to Freedom TCP tunnel. Acts as the **helper** — listens on a local TCP port and tunnels connections to the adapter through YC.


## Supported platforms

- **Android** (API 26+ / Android 8.0+)
- **iOS** (15.0+)
- **Windows** (10 1809+)
- **macOS** (via Mac Catalyst, macOS 12+)
- **Linux** (via the GTK4 backend — see [Linux build](#linux-build) below)

## Requirements

- .NET 10 SDK (only thing required for the Linux head)
- MAUI workload (`dotnet workload install maui`) — needed for Android / iOS / macOS / Windows targets, **but NOT for the Linux head** (`maui` metapackage isn't even installable on Linux because it pulls in ios/maccatalyst). The Linux head builds straight from NuGet packages.
- For Android: Android SDK (installed automatically with the MAUI workload)
- For iOS/macOS: a Mac with Xcode
- For Linux: GTK4 + libadwaita + WebKitGTK runtime libs on the target machine (see [Linux build](#linux-build))

## Important

If you use a proxy client that works as a TUN, you must add an exclusion for the helper process, otherwise you'll get an infinite loop and nothing will work. On connect, the app logs endpoint domains and IP addresses — you can add those to exclusions if per-process exclusions aren't available. That said, in the Happ client it didn't work for me even with that (but it works fine without TUN, e.g. for Telegram).

## Build

```bash
cd client

# Android (Debug)
dotnet build -f net10.0-android

# Android (Release APK)
dotnet publish -f net10.0-android -c Release
# The Android APK will be in `bin/Release/net10.0-android/publish/`.

# iOS (requires a Mac with Xcode)
dotnet build -f net10.0-ios

# Windows
dotnet build -f net10.0-windows10.0.19041.0

# macOS (Mac only)
dotnet build -f net10.0-maccatalyst

# Linux
cd ../maui-client-linux
dotnet publish -c Release -r linux-x64 --self-contained -o publish/linux-x64
# -> publish/linux-x64/BridgeToFreedom.Linux  (the executable)
```

## Linux build

Linux support uses the GTK4 backend from [dotnet/maui-labs](https://github.com/dotnet/maui-labs/tree/main/platforms/Linux.Gtk4) (NuGet: [`Microsoft.Maui.Platforms.Linux.Gtk4`](https://www.nuget.org/packages/Microsoft.Maui.Platforms.Linux.Gtk4) + `.Essentials`). The Linux head project lives at [`maui-client-linux/`](../maui-client-linux/) and references the shared MAUI project as a `net10.0` library — the same App / MainPage / TunnelService code runs on every platform.


### Build & run on Linux


Install the GTK4 / libadwaita / WebKitGTK runtime libs (the GTK4 backend P/Invokes them on startup — without them the app throws `DllNotFoundException`):

> **Required: GTK 4.12+** — the Microsoft GTK4 backend P/Invokes `gtk_css_provider_load_from_string`, added in GTK 4.12 (Sep 2023). On older GTK the app crashes at first render with `EntryPointNotFoundException`. Check with `pkg-config --modversion gtk4` or `dpkg -s libgtk-4-1 | grep Version`.
>
> **Distros that work out of the box (GTK 4.12+):** Ubuntu 24.04+, Debian 13 (trixie)+, Fedora 40+, RHEL 9+, Arch, openSUSE Tumbleweed.
> **Distros that DO NOT work** (ship GTK < 4.12): Ubuntu 22.04 LTS (GTK 4.6), Debian 12 bookworm (GTK 4.8).

| Distro | Command |
| --- | --- |
| **Debian 13+ / Ubuntu 24.04+ / Mint 22+ / WSL** | `sudo apt install -y libgtk-4-1 libadwaita-1-0 libwebkitgtk-6.0-4 libgirepository-1.0-1 gsettings-desktop-schemas` |
| **Fedora 40+ / RHEL 9+** | `sudo dnf install -y gtk4 libadwaita webkitgtk6.0 gobject-introspection glib2 cairo pango` |
| **Arch / Manjaro** | `sudo pacman -S --needed gtk4 libadwaita webkitgtk-6.0 gobject-introspection glib2 cairo pango` |
| **openSUSE Tumbleweed** | `sudo zypper install gtk4 libadwaita webkitgtk-6_0 gobject-introspection glib2 cairo pango` |


## Usage

1. Enter **Bridge URL** — the helper endpoint of the API Gateway (e.g. `wss://gateway.example.com/_helper`)
2. Enter **Auth Token** — the shared secret matching the adapter and Cloud Function
3. Set **Listen Address** and **Port** — where clients should connect (default `127.123.45.67:5080`). If you want to share the tunnel with other devices on your local network, change the address to `0.0.0.0` manually so the listener binds on all interfaces.
4. If your device cannot reach the `wsSend` API directly, enable **Relay mode**. Recommendation: don't enable relay mode unless you have to — only use it if direct mode doesn't work for you. Direct mode is faster and more stable.
5. Press **CONNECT**

The app keeps the tunnel running in the background:
- **Android**: foreground service with a wake lock
- **iOS**: `beginBackgroundTask` + `BGProcessingTask`

Press **DISCONNECT** to stop.

Settings are saved automatically and restored on next launch; they can also be imported/exported as a URL.

## Recommended setup for Android (proven stable)

The combination below has proven both effective and stable:

- On the phone: install this app (BTF) and [v2rayNG](https://github.com/2dust/v2rayNG). In v2rayNG, enable per-app proxying and pick the apps you actually want to route through the tunnel (e.g. Chrome and Telegram only — important so v2rayNG does NOT try to route BTF's own upstream traffic, which would cause a loop). Create a new outbound profile of type SOCKS (or VLESS) and point it to `127.123.45.67:5080`. Start BTF first and connect to the server, then enable v2rayNG.
- On the adapter side (VPS): run Dante (SOCKS) or XRay (VLESS) listening on whatever address the adapter forwards to (i.e. the adapter's `target.address`). The adapter delivers each incoming TCP stream to it and the proxy then exits to the open internet.

Flow: `app → v2rayNG (per-app) → BTF helper :5080 → YC → BTF adapter → Dante/XRay → internet`.
