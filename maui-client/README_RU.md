# Bridge to Freedom — Клиентское приложение

Кроссплатформенный GUI-клиент для TCP-туннеля Bridge to Freedom. Работает как **helper** — слушает локальный TCP-порт и туннелирует соединения к адаптеру через Yandex Cloud.

## Поддерживаемые платформы

- **Android** (API 26+ / Android 8.0+)
- **iOS** (15.0+)
- **Windows** (10 1809+)
- **macOS** (через Mac Catalyst, macOS 12+)
- **Linux** — используйте Go-бинарник `helper` (`adapter/cmd/helper`)

## Требования

- .NET 10 SDK
- MAUI workload: `dotnet workload install maui`
- Для Android: Android SDK (устанавливается автоматически с MAUI workload)
- Для iOS/macOS: Mac с Xcode

## Важно

Если вы используете какой-нибудь прокси-клиент, который работает как TUN, то надо обязательно настроить исключение для процесса хелпера, иначе будет бесконечный цикл и ничего не заработает. Приложение при подключении пишет в лог домены и IP-адреса эндпоинтов, можно добавить их в исключения, если исключения по процессам недоступны - правда, в Happ-клиенте у меня оно все равно не заработало (но работает неплохо без TUN, например для того же TG).

## Сборка

```bash
cd client

# Android (Debug)
dotnet build -f net10.0-android

# Android (Release APK)
dotnet publish -f net10.0-android -c Release

# iOS (требуется Mac с Xcode)
dotnet build -f net10.0-ios

# Windows
dotnet build -f net10.0-windows10.0.19041.0

# macOS (только на Mac)
dotnet build -f net10.0-maccatalyst
```

APK для Android будет в `bin/Release/net10.0-android/publish/`.

## Использование

1. Введите **Bridge URL** — адрес helper-эндпоинта API Gateway (например `wss://gateway.example.com/_helper`)
2. Введите **Auth Token** — общий секрет, совпадающий с адаптером и Cloud Function
3. Укажите **Listen Address** и **Port** — адрес для подключения клиентов (по умолчанию `127.0.0.1:1080`)
4. При необходимости включите **Relay mode**, если устройство не имеет доступа к API wsSend напрямую
5. Нажмите **CONNECT**

Приложение поддерживает туннель в фоне:
- **Android**: foreground service с wake lock
- **iOS**: `beginBackgroundTask` + `BGProcessingTask`

Нажмите **DISCONNECT** для остановки.

Настройки сохраняются автоматически и восстанавливаются при следующем запуске, плюс можно их импортировать-экспортировать как URL.
