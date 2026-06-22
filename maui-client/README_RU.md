# Bridge to Freedom — Клиентское приложение

Кроссплатформенный GUI-клиент для TCP-туннеля Bridge to Freedom. Работает как **helper** — слушает локальный TCP-порт и туннелирует соединения к адаптеру через Yandex Cloud.

## Поддерживаемые платформы

- **Android** (API 26+ / Android 8.0+)
- **iOS** (15.0+)
- **Windows** (10 1809+)
- **macOS** (через Mac Catalyst, macOS 12+)
- **Linux** (см. раздел [Сборка под Linux](#сборка-под-linux))

## Требования

- .NET 10 SDK
- MAUI workload (`dotnet workload install maui`) — нужен для Android / iOS / macOS / Windows-сборок, **но НЕ нужен для Linux-head** (сам мета-workload `maui` на Linux вообще не ставится — он тянет ios/maccatalyst). Linux-head собирается напрямую из NuGet-пакетов.
- Для Android: Android SDK (устанавливается автоматически с MAUI workload)
- Для iOS/macOS: Mac с Xcode
- Для Linux: на целевой машине нужны runtime-библиотеки GTK4 + libadwaita + WebKitGTK (см. [Сборка под Linux](#сборка-под-linux))

## Важно

Если вы используете какой-нибудь прокси-клиент, который работает как TUN, то надо обязательно настроить исключение для процесса хелпера, иначе будет бесконечный цикл и ничего не заработает. Приложение при подключении пишет в лог домены и IP-адреса эндпоинтов, можно добавить их в исключения, если исключения по процессам недоступны - правда, в Happ-клиенте у меня оно все равно не заработало (но работает неплохо без TUN, например для того же TG).

## Сборка

```bash
cd client

# Android (Debug)
dotnet build -f net10.0-android

# Android (Release APK)
dotnet publish -f net10.0-android -c Release
# APK для Android будет в `bin/Release/net10.0-android/publish/`.

# iOS (требуется Mac с Xcode)
dotnet build -f net10.0-ios

# Windows
dotnet build -f net10.0-windows10.0.19041.0

# macOS (только на Mac)
dotnet build -f net10.0-maccatalyst

cd maui-client-linux
dotnet publish -c Release -r linux-x64 --self-contained -o publish/linux-x64
# -> publish/linux-x64/BridgeToFreedom.Linux  (исполняемый файл)
```

## Сборка под Linux

Поддержка Linux работает через бэкенд GTK4 из [dotnet/maui-labs](https://github.com/dotnet/maui-labs/tree/main/platforms/Linux.Gtk4) (NuGet: [`Microsoft.Maui.Platforms.Linux.Gtk4`](https://www.nuget.org/packages/Microsoft.Maui.Platforms.Linux.Gtk4) + `.Essentials`). Linux-head проект лежит в [`maui-client-linux/`](../maui-client-linux/) и ссылается на основной MAUI-проект как на `net10.0`-библиотеку — один и тот же код App / MainPage / TunnelService работает на всех платформах.

### Сборка и запуск на Linux

Поставьте runtime-библиотеки GTK4 / libadwaita / WebKitGTK (бэкенд GTK4 P/Invoke'ит их при старте — без них приложение падает с `DllNotFoundException`):

> **Нужна GTK 4.12+** 
> **Дистрибутивы, где всё работает из коробки (GTK 4.12+):** Ubuntu 24.04+, Debian 13 (trixie)+, Fedora 40+, RHEL 9+, Arch, openSUSE Tumbleweed.
> **Дистрибутивы, где НЕ работает** (в репах GTK < 4.12): Ubuntu 22.04 LTS (GTK 4.6), Debian 12 bookworm (GTK 4.8).

| Дистрибутив | Команда |
| --- | --- |
| **Debian 13+ / Ubuntu 24.04+ / Mint 22+ / WSL** | `sudo apt install -y libgtk-4-1 libadwaita-1-0 libwebkitgtk-6.0-4 libgirepository-1.0-1 gsettings-desktop-schemas` |
| **Fedora 40+ / RHEL 9+** | `sudo dnf install -y gtk4 libadwaita webkitgtk6.0 gobject-introspection glib2 cairo pango` |
| **Arch / Manjaro** | `sudo pacman -S --needed gtk4 libadwaita webkitgtk-6.0 gobject-introspection glib2 cairo pango` |
| **openSUSE Tumbleweed** | `sudo zypper install gtk4 libadwaita webkitgtk-6_0 gobject-introspection glib2 cairo pango` |

## Использование

1. Введите **Bridge URL** — адрес helper-эндпоинта API Gateway (например `wss://gateway.example.com/_helper`)
2. Введите **Auth Token** — общий секрет, совпадающий с адаптером и Cloud Function
3. Укажите **Listen Address** и **Port** — адрес для подключения клиентов (по умолчанию `127.123.45.67:5080`). Если хотите раздать туннель другим устройствам в локальной сети — поменяйте адрес на `0.0.0.0` вручную, тогда слушатель будет открыт на всех интерфейсах.
4. При необходимости включите **Relay mode**, если устройство не имеет доступа к API wsSend напрямую. Рекомендация: не включайте relay-режим без необходимости — используйте его только если без него не работает. Прямой режим быстрее и стабильнее.
5. Нажмите **CONNECT**

Приложение поддерживает туннель в фоне:
- **Android**: foreground service с wake lock
- **iOS**: `beginBackgroundTask` + `BGProcessingTask`

Нажмите **DISCONNECT** для остановки.

Настройки сохраняются автоматически и восстанавливаются при следующем запуске, плюс можно их импортировать-экспортировать как URL.

## Рекомендуемая схема для Android (проверено, стабильно)

Связка ниже показала себя эффективной и стабильной:

- На телефоне: ставите это приложение (BTF) и [v2rayNG](https://github.com/2dust/v2rayNG). В v2rayNG включаете per-app проксирование и выбираете именно те приложения, которые хотите гонять через туннель (например, только Chrome и Telegram — это важно, чтобы v2rayNG НЕ пытался завернуть собственный upstream-трафик BTF, иначе получится цикл). Создаёте новый outbound-профиль типа SOCKS (или VLESS) и указываете адрес `127.123.45.67:5080`. Сначала запускаете BTF и подключаетесь к серверу, потом включаете v2rayNG.
- На стороне адаптера (VPS): поднимаете Dante (SOCKS) или XRay (VLESS), слушающий на адресе, в который адаптер форвардит трафик (т.е. `target.address` в конфиге адаптера). Адаптер отдаёт каждый TCP-stream в этот прокси, а тот уже выходит в открытый интернет.

Цепочка: `приложение → v2rayNG (per-app) → BTF helper :5080 → YC → BTF adapter → Dante/XRay → интернет`.
