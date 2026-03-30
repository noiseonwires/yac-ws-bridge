# Bridge to Freedom

Прокси-туннель через Yandex Cloud.
Существует в двух вариантах:
1. вариант с одним адаптером (бранч one-adapter): проксирует websocket-подключения (VLESS c WS транспортом или XMPP-over-websockets, например), не требует модификации клиентов или установки дополнительного софта на клиентское устройство - просто указываете в  URL для websocket-подключения адрес serverless-функции. Работает медленно и очень нестабильно. См. README_RU.md в бранче one-adapter для подробностей.

2. вариант с адаптером+хелпером (бранч main) на сервере и на клиенте. Проксирует любые TCP-подключения и работает гораздо стабильнее и быстрее.

В варианте с адаптером+хелпером

Два Go-бинарника — **adapter** (на стороне целевого сервера) и **helper** (на стороне клиента) — каждый держит upstream WebSocket-соединение к YC API Gateway. В режиме "без relay" данные передаются **напрямую** через YC WebSocket management API (gRPC `wsSend`), минуя Serverless Function на пути данных. Serverless Function нужна только чтобы клиент и сервер (хелпер и адаптер) нашли друг друга.

В хороших условиях (адаптер в РФ рядом с Яндексом) получается выжать до 20 мегабит.

Также доступно кроссплатформенное **MAUI-приложение** (Android, iOS, Windows, macOS) как GUI-альтернатива Go-хелперу — см. [maui-client/README_RU.md](maui-client/README_RU.md).

> **Примечание:** v4 туннелирует сырые TCP-потоки. Префиксы путей (доступные в одноадаптерной версии v3 для WebSocket-проксирования) не поддерживаются.

```
                            Yandex Cloud
                     ┌─────────────────────────┐
Client ──TCP──► Helper ──wsSend(gRPC)──► API Gateway ──WS──► Adapter ──TCP──► Target
                     │                         │
Client ◄──TCP── Helper ◄──WS────────── API Gateway ◄──wsSend(gRPC)── Adapter ◄──TCP── Target
                     │                         │
                     │    Cloud Function       │
                     │    (discovery only)     │
                     └─────────────────────────┘
```

## Как это работает

1. **Adapter** подключается исходящим соединением к API Gateway по пути `/_adapter` и аутентифицируется у Serverless Function (HELLO-рукопожатие). Получает свой connection ID.
2. **Helper** подключается по пути `/_helper`, аутентифицируется и получает connection ID адаптера. Serverless Function также уведомляет адаптер об ID хелпера.
3. **Клиент** открывает TCP-соединение на порт хелпера. Хелпер назначает stream ID и отправляет фрейм OPEN адаптеру через `wsSend`.
4. **Adapter** открывает TCP-соединение к целевому сервису, отвечает OPEN_OK.
5. Данные передаются в обе стороны: TCP → helper → wsSend → adapter → TCP (и обратно).
6. При закрытии TCP с любой стороны отправляется фрейм FIN для закрытия соответствующего потока.

Все TCP-потоки мультиплексируются поверх двух upstream WebSocket-соединений (по одному в каждом направлении).

### Переупорядочивание (reorder)

В режиме с relay фреймы одного потока могут приходить не по порядку. Адаптер автоматически переупорядочивает входящие фреймы по `SeqID`: фреймы, пришедшие раньше ожидаемого, буферизуются и отдаются приложению в правильном порядке. Каких-либо конфигурационных параметров для этого нет — механизм включён в адаптере всегда.

### Объединение пакетов (write coalescing)

Параметр `writeCoalescing` реализует алгоритм, аналогичный TCP Nagle: мелкие TCP-чтения (read) буферизуются и объединяются в один DATA-фрейм. Это значительно уменьшает количество вызовов `wsSend`/relay-сообщений, что повышает пропускную способность и уменьшает нагрузку на Yandex Cloud. Данные отправляются либо по истечении `delayMs`, либо при достижении буфером 32 КБ — что наступит раньше. При использовании go-хелпера, параметр рекомендуется включить на стороне адаптера всегда при использовании режима relay, а без него - опционально на обеих сторонах (попробовать, может быть лучше с ним, может быть лучше без него). В MAUI-приложении оно глючное.

**Важно**: один адаптер - одна функция - один клиент!
Несколько клиентов к одной функции/адаптеру - undefined behavior (все сломается). Чтобы могло работать несколько клиентов, надо немного переделать протокол и реализацию, но мне лень.

### Режим relay

Если хелпер не может достучаться до gRPC-эндпоинта `wsSend` (API Яндекс-облака, например, в ограниченной сети), установите `wsApi.relay: true`. Хелпер отправляет данные через свой upstream WebSocket, и Serverless Function ретранслирует их адаптеру. Обратный путь (adapter → helper) по-прежнему использует `wsSend` напрямую. Режим relay работает медленнее и не так стабильно по тем же причинам, что и вариант с одним адаптером (one-branch), но все-таки стабильнее за счет нормального мультиплексирования.

--

## Важное
Через сколько вас Яндекс за такое забанит - я без понятия. 

Рекомендации:

- Использовать это только для проксирования самых важных сервисов с небольшим трафиком (например, прокинуть туннель до SOCKS-прокси и вбить его в Telegram как 127.0.0.1), не злоупотребять гоняя большие объемы данных

- Перед загрузкой кода serverless function, прогнать его любым Javascript-обфускатором, можно даже пару раз. В самом коде serverless-функции, адаптера и хелпера заменить все стандартные пути (/_upstream, /_helper, /_conn-ids) на свои рандомные.

### И еще важное

Если вы используете какой-нибудь прокси-клиент, который работает как TUN, то надо обязательно настроить исключение для процесса хелпера, иначе будет бесконечный цикл и ничего не заработает. Мобильное приложение (MAUI) при подключении пишет в лог домены и IP-адреса эндпоинтов, можно добавить их в исключения, если исключения по процессам недоступны - правда, в Happ-клиенте у меня оно все равно не заработало (но работает неплохо без TUN, например для того же TG).

---

## Adapter

Ставится на какой-нибудь VPS. В идеале, тоже в РФ, поближе к Яндексу, а дальше с него проксируйтесь уже куда угодно и как угодно.

### Сборка

Требуется Go 1.21+.

```bash
cd adapter
go build -o adapter ./cmd/adapter
```

### Конфигурация

Создайте `adapter.config.yaml`:

```yaml
bridge:
  url: "wss://<домен-api-gateway>/_adapter"
  authToken: "<общий-секрет>"
  reconnect:
    initialDelayMs: 1000
    maxDelayMs: 30000
    backoffMultiplier: 2
  pingIntervalMs: 30000

target:
  address: "127.0.0.1:9090"

http:
  listenPort: 3001

writeCoalescing:
  enabled: true
  delayMs: 50

wsApi:
  mode: "grpc"

logging:
  level: "info"
```

| Ключ | Описание |
|------|----------|
| `bridge.url` | WebSocket URL эндпоинта API Gateway для адаптера |
| `bridge.authToken` | Общий секрет (должен совпадать с `AUTH_TOKEN` в Cloud Function) |
| `bridge.reconnect` | Экспоненциальный backoff для переподключения upstream |
| `bridge.pingIntervalMs` | Интервал PING для предотвращения idle-отключения (держите менее 10 мин) |
| `target.address` | TCP-адрес целевого сервиса |
| `http.listenPort` | HTTP-порт для эндпоинта `/conn-ids` (используется Cloud Function при холодном старте) |
| `writeCoalescing.enabled` | Включить объединение мелких пакетов в один фрейм (аналог алгоритма Нейгла). Уменьшает количество вызовов `wsSend` и повышает пропускную способность |
| `writeCoalescing.delayMs` | Максимальная задержка буферизации перед отправкой (мс). Данные отправляются раньше, если буфер достигает 32 КБ. Рекомендуемое значение: 10–100 мс |
| `wsApi.mode` | `grpc` (единственный вариант в v4) |

### Запуск

```bash
./adapter adapter.config.yaml
```

### HTTP-эндпоинты

| Эндпоинт | Метод | Описание |
|-----------|-------|----------|
| `/conn-ids` | GET | Возвращает `{"adapterConnId":"...","helperConnId":"..."}`. Авторизация: `Bearer <authToken>`. |

---

## Helper

Ставится на клиентское устройство.

Кроме консольной Go-версии, есть так же красивое приложение (.NET MAUI под Windows, MacOS, Android, iOS) с тем же функционалом. См. папку 'maui-client'.

### Сборка

```bash
cd adapter-and-helper
go build -o helper ./cmd/helper
```

### Конфигурация

Создайте `helper.config.yaml`:

```yaml
bridge:
  url: "wss://<домен-api-gateway>/_helper"
  authToken: "<общий-секрет>"
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

| Ключ | Описание |
|------|----------|
| `listen.address` | TCP-адрес для прослушивания клиентских подключений |
| `writeCoalescing.enabled` | Включить объединение мелких пакетов (см. описание выше в секции адаптера) |
| `writeCoalescing.delayMs` | Задержка буферизации перед отправкой (мс). Рекомендуемое значение: 10–100 мс |
| `wsApi.relay` | `true` = отправлять данные через upstream WS (Cloud Function ретранслирует). `false` = отправлять через gRPC `wsSend` напрямую. |

### Запуск

```bash
./helper helper.config.yaml
```

Клиенты подключаются к `listen.address` по обычному TCP. Каждое соединение туннелируется к целевому сервису.

---

## Развёртывание Serverless Function (Yandex Cloud)

### Предварительные требования

- Аккаунт Yandex Cloud с подключённым биллингом
- Установленный и настроенный `yc` CLI (`yc init`)
- Каталог (folder) в облаке для проекта

### 1. Создание сервисного аккаунта

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

### 2. Создание и деплой Serverless Function

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
  --environment "AUTH_TOKEN=<ваш-общий-секрет>,ADAPTER_URL=<url-http-адаптера>"
```

| Переменная | Обязательна | Описание |
|------------|-------------|----------|
| `AUTH_TOKEN` | Да | Общий секрет (то же значение, что `bridge.authToken`) |
| `ADAPTER_URL` | Да | HTTP(S) URL адаптера (например `https://your-server:3001`). Используется для получения `/conn-ids` при холодном старте; необходим для восстановления состояния при нескольких инстансах. |

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

Укажите `bridge.url` в `adapter.config.yaml` как `wss://<GW_DOMAIN>/_adapter`.

```bash
cd adapter-and-helper
go build -o adapter ./cmd/adapter
./adapter adapter.config.yaml
```

### 5. Настройка и запуск хелпера

Укажите `bridge.url` в `helper.config.yaml` как `wss://<GW_DOMAIN>/_helper`.

```bash
cd adapter-and-helper
go build -o helper ./cmd/helper
./helper helper.config.yaml
```

### 6. Подключение клиентов

Направьте любой TCP-клиент на адрес прослушивания хелпера:

```bash
# Пример: проксирование SSH через туннель
ssh -o ProxyCommand="nc 127.0.0.1 1080" user@target
```

### 6. Просмотр логов Serverless Function
Просмотр логов Cloud Function:
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