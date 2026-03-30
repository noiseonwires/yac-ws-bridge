# Bridge to Freedom

Прокси-туннель через Yandex Cloud.
В этом бранче вариант с одним адаптером: проксирует websocket-подключения (VLESS c WS транспортом или XMPP-over-websockets, например), не требует модификации клиентов или установки дополнительного софта на клиентское устройство - просто указываете в  URL для websocket-подключения адрес serverless-функции.
Serverless-функция пересылает данные туда-сюда.

## Как оно работает в реальности
Основная идея - использовать туннель совместно с VLESS-over-Websocket.
Работает хреново. По ощущениям - как во времена dial-up'а :)
Из-за особенной работы Serverless Function Яши и API Gateway, пакеты иногда приходят в неправильном порядке (я пробовал делать reordering в адаптере с короткой буферизацией, стало почему-то только хуже), либо же иногда вообще теряются (в том числе CONNECT-запросы).
Потратив час на попытки разобраться с причиной, забил и переключился на версию с адаптером + хелпером (бранч main).

Поэтому серфить интернет кое-как можно, но часто TLS-соединения обрываются на этапе хендшейка, и чтобы сайты загружались целиком приходится по несколько раз обновлять страницу. Сайты, где все ресурсы грузятся с одного сервера и которые хорошо попадают в кэш, через какое-то время смотреть можно даже весьма комфортно, хоть и неторопливо.
Мобильные приложения - какие как, например вполне можно читать Reddit, но тоже часто приходиться тапать на Refresh если не загрузился пост или комменты. Зато внезапно неплохо работает Telegram, он часто сваливается в Connecting/Updating, но сообщения доходят исправно, и даже картинки/видео можно смотреть. Видимо, у них протокол хорошо оптимизирован для нестабильной связи. XMPP-over-websocket с некоторыми клиентами (патченный Conversations) работает тоже весьма неплохо.

Рекомендации для VLESS-over-WS:
Ставьте ?ed=8000 или что-то подобное в path в XRay, оно очень сильно улучшает работу. MUX, в теории, мог бы помочь, но с ним вообще ничего не работает - пробовал и mux.cool из XRay, и h2mux/smux/yamux из Sing-box - нет и все (подозреваю, что они там делают очень много мелких write() в сокет, которые летят как отдельные ws-сообщения, и в итоге повышается шанс потери их по пути или неправильного порядка).


## Важное
Через сколько вас Яндекс за такое забанит - я без понятия. 

Рекомендации:

- Использовать это только для проксирования самых важных сервисов с небольшим трафиком (например, прокинуть туннель до SOCKS-прокси и вбить его в Telegram как 127.0.0.1), не злоупотребять гоняя большие объемы данных

- Перед загрузкой кода serverless function, прогнать его любым Javascript-обфускатором, можно даже пару раз. В самом коде serverless-функции, адаптера и хелпера заменить все стандартные пути (/_upstream, /_helper, /_conn-ids) на свои рандомные.


## Адаптер

Ставится на какой-нибудь VPS. В идеале, тоже в РФ, поближе к Яндексу, а дальше с него проксируйтесь уже куда угодно и как угодно.


### Сборка

Требуется Go 1.21+.

```bash
cd adapter
go build -o adapter ./cmd/adapter
```

### Конфигурация

Создайте файл `adapter.config.yaml` (пример есть в репозитории):

```yaml
bridge:
  url: "wss://<домен-api-gateway>/_upstream"
  authToken: "<общий-секрет>"
  reconnect:
    initialDelayMs: 1000
    maxDelayMs: 30000
    backoffMultiplier: 2
  pingIntervalMs: 30000

wakeup:
  listenPort: 3001
  # pathPrefix: "/myprefix"  # опционально, должен совпадать с путём в ADAPTER_URL облачной функции

target:
  url: "ws://127.0.0.1:9090"

wsApi:
  mode: "grpc"          # "rest" или "grpc" - grpc быстрее

logging:
  level: "info"
```

| Раздел | Ключ | Описание |
|--------|------|----------|
| **bridge.url** | | WebSocket URL upstream-эндпоинта API Gateway |
| **bridge.authToken** | | Общий секрет (должен совпадать с переменной `AUTH_TOKEN` облачной функции) |
| **bridge.reconnect** | | Параметры экспоненциального backoff для переподключения |
| **bridge.pingIntervalMs** | | Интервал отправки PING-фреймов для предотвращения idle-отключения |
| **wakeup.listenPort** | | HTTP-порт для wakeup/fallback-эндпоинта (0 — отключить) |
| **wakeup.pathPrefix** | | Опциональный префикс URL для всех HTTP-эндпоинтов (например `/myprefix`). Должен совпадать с путём в `ADAPTER_URL` облачной функции. |
| **wakeup.tlsCert / tlsKey** | | Опциональный TLS для wakeup-эндпоинта |
| **target.url** | | WebSocket URL целевого сервера (обычно `ws://127.0.0.1:<порт>`) |
| **wsApi.mode** | | Способ вызова YC management API: `rest` или `grpc` |

### Запуск

```bash
./adapter adapter.config.yaml
```

Адаптер подключится к bridge, пройдёт аутентификацию и начнёт принимать клиентский трафик. Также запускается HTTP-сервер на wakeup-порту со следующими эндпоинтами:

| Эндпоинт | Метод | Назначение |
|-----------|-------|------------|
| `/` | POST | Приём протокольного фрейма (POST fallback) и инициирование переподключения upstream |
| `/upstream-id` | GET | Возвращает текущий ID upstream-соединения (используется функцией при холодном старте) |
| `/proxy` | POST | Проксирует простой HTTP-запрос к целевому серверу (используется при включённой опции `FORWARD_HTTP`) |

---

## Развёртывание Cloud Function (Yandex Cloud)

### Предварительные требования

- Аккаунт Yandex Cloud с подключённым биллингом
- Установленный и настроенный `yc` CLI (`yc init`)
- Каталог (folder) в облаке для проекта

### 1. Создание сервисного аккаунта

```bash
yc iam service-account create --name bridge-sa

SA_ID=$(yc iam service-account get bridge-sa --format json | jq -r .id)
FOLDER_ID=$(yc config get folder-id)

# Разрешить SA вызывать функции и отправлять WebSocket-сообщения
yc resource-manager folder add-access-binding $FOLDER_ID \
  --role serverless.functions.invoker \
  --subject serviceAccount:$SA_ID

yc resource-manager folder add-access-binding $FOLDER_ID \
  --role api-gateway.websocketBroadcaster \
  --subject serviceAccount:$SA_ID
```

### 2. Создание и деплой Cloud Function

```bash
cd bridge-cloud
zip -r bridge-function.zip index.js package.json
```

```bash
yc serverless function create --name bridge-fn

FUNCTION_ID=$(yc serverless function get bridge-fn --format json | jq -r .id)
```

Деплой версии:

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
  --environment "AUTH_TOKEN=<ваш-общий-секрет>,ADAPTER_URL=<url-wakeup-адаптера>"
```

| Переменная | Обязательна | Описание |
|------------|-------------|----------|
| `AUTH_TOKEN` | Да | Общий секрет (то же значение, что `bridge.authToken` в конфигурации адаптера) |
| `ADAPTER_URL` | Нет | HTTP(S) URL wakeup-эндпоинта адаптера (например `https://your-server:3001`). Поддерживает префикс пути (например `https://your-server:3001/myprefix`) — все HTTP-вызовы к адаптеру будут использовать этот префикс. Должен совпадать с `wakeup.pathPrefix` в конфигурации адаптера. Включает POST fallback и восстановление при холодном старте. |
| `FORWARD_HTTP` | Нет | Установите `true` для проксирования обычных HTTP-запросов (GET, POST и т.д.) через адаптер к целевому серверу. При включении любой не-WebSocket запрос к API Gateway пересылается на целевой сервер, а ответ возвращается клиенту. |

### 3. Создание API Gateway

Отредактируйте `bridge-cloud/spec.yaml` — замените два плейсхолдера:

- `${FUNCTION_ID}` — ID функции из шага 2
- `${SERVICE_ACCOUNT_ID}` — ID сервисного аккаунта из шага 1

Затем создайте gateway:

```bash
yc serverless api-gateway create \
  --name bridge-gw \
  --spec spec.yaml
```

Получите домен gateway:

```bash
GW_DOMAIN=$(yc serverless api-gateway get bridge-gw --format json | jq -r .domain)
echo "Gateway: wss://$GW_DOMAIN"
```

### 4. Настройка и запуск адаптера

Укажите `bridge.url` в `adapter.config.yaml` как `wss://<GW_DOMAIN>/_upstream`, а `bridge.authToken` — тот же секрет, что и в `AUTH_TOKEN`.

```bash
cd adapter
go build -o adapter ./cmd/adapter
./adapter adapter.config.yaml
```

### 5. Подключение клиентов

Клиенты подключаются к API Gateway по любому пути:

```
wss://<GW_DOMAIN>/your/path
```

Путь передаётся на целевой сервер как есть, то есть `wss://gw.example.com/chat` приведёт к подключению адаптера к `ws://127.0.0.1:9090/chat`.

Просмотр логов:
```bash
yc logging read --folder-id $(yc config get folder-id) --follow
```

---

## Лимиты Yandex Cloud

| Лимит | Значение |
|-------|----------|
| Максимальное время жизни WebSocket-соединения | 60 минут |
| Таймаут бездействия (нет сообщений) | 10 минут |
| Максимальный размер сообщения | 128 КБ |
| Максимальный размер фрейма | 32 КБ |
| Таймаут выполнения функции | 10 с (настраивается) |

Устанавливайте `pingIntervalMs` менее 10 минут, чтобы избежать отключения по бездействию.

## Лицензия

WTFPL, cм. [LICENSE](LICENSE).
Дальшнейших доработок  не будет, поддержки и ответов на вопросы - тоже. Goodbye and kiss my ass.