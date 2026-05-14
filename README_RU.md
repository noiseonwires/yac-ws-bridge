# Bridge to Freedom — IP-туннель

Прокси-туннель через Yandex Cloud, на этой ветке — **в виде IP-VPN**: на каждой стороне поднимается TUN-устройство, между ними по WebSocket'у через YC API Gateway гоняются сырые IP-пакеты. Переупорядочивание и переотправку обрабатывает обычный TCP-стек ОС, поэтому никакой прикладной мультиплексор/реордеринг больше не нужен.

> ⚠️ **Эта реализация живёт в отдельном бранче `ip-tun`.** Перед сборкой переключитесь: `git checkout ip-tun`. На `main` и других ветках — другие (несовместимые) wire-протоколы.

## TL;DR — что реально работает

- **Это не полноценный VPN.** Пропускная способность на практике сравнима со старым 2G/EDGE. Серфить веб больно.
- **Зато прекрасно работает Telegram.** Протокол Telegram любит мелкие сообщения и устойчив к задержкам — туннелю это как раз подходит. Чаты, голосовые сообщения, мелкие картинки/стикеры работают вполне юзабельно.
- **Рекомендуемая схема**: поставьте Android-приложение, включите **per-app tunneling** и пустите через туннель **только Telegram** (плюс, возможно, ещё одно-два чувствительных к цензуре и нетребовательных по трафику приложения). Всё остальное оставьте на обычной сети.

Два Go-бинарника — **adapter** (на стороне целевого сервера, обычно VPS в РФ рядом с Яндексом) и **helper** (на клиенте) — каждый поднимает TUN-устройство и держит upstream WebSocket к YC API Gateway. Ретрансмиты, переупорядочивание, congestion control — всё на TCP-стеке ОС, никакого прикладного мультиплексора больше нет. Также есть **MAUI-приложение под Android**, делающее то же самое через `VpnService`, с **per-app tunneling** — можно пускать в туннель только выбранные приложения.

В IP-туннельном варианте этого бранча:

Два Go-бинарника — **adapter** (на стороне целевого сервера, обычно VPS в РФ рядом с Яндексом) и **helper** (на клиенте) — каждый поднимает TUN-устройство и держит upstream WebSocket к YC API Gateway. В режиме без relay IP-пакеты идут **напрямую** через YC WebSocket management API (gRPC `wsSend`), минуя Cloud Function на пути данных. Cloud Function нужна только чтобы хелпер и адаптер нашли друг друга.

```
                                       Yandex Cloud
                          ┌─────────────────────────────────┐
[приложения] ──► TUN ──► Helper ──wsSend(gRPC)──► API GW ──WS──► Adapter ──► TUN ──► (ядро роутит/NATит дальше)
                          ▲                       │           ▲
[приложения] ◄── TUN ◄── Helper ◄──WS── API GW ◄─wsSend(gRPC)─ Adapter ◄── TUN ◄── (ответные пакеты)
                                          │
                                  Cloud Function
                                  (только discovery)
```

## Как это работает

1. **Adapter** подключается к API Gateway по `/_adapter`, аутентифицируется у Cloud Function (HELLO) и получает свой connection ID. Поднимает локальный TUN с `/30`-эндпоинтом (например `10.200.0.1`).
2. **Helper** подключается по `/_helper`, аутентифицируется, узнаёт ID адаптера и поднимает свой TUN с противоположным эндпоинтом (`10.200.0.2`). Cloud Function уведомляет адаптер об ID хелпера.
3. Каждая сторона читает сырые IP-пакеты со своего TUN, заворачивает каждый в кадр `PACKET` (или батч `PACKET_BATCH`) и отправляет пиру через `wsSend` (или через relay в Cloud Function).
4. Принимающая сторона декодирует кадр и пишет пакет в свой TUN. Дальше всё делает ядро. Никакого per-stream-состояния, реордеринга или FIN/RST на уровне моста.

Поскольку TCP-стек ОС на обеих сторонах сам обеспечивает переупорядочивание и ретрансмит, дропнутые или пришедшие не по порядку кадры теперь не проблема — TCP всё догонит сам. (UDP-пакеты теряются как обычно — это нормальное поведение UDP.)

### Wire-протокол (очень маленький)

`[1B type][payload...]`

| Тип | Hex  | Направление | Payload |
|---|---|---|---|
| HELLO        | `0x01` | →  | `[1B version][token]` |
| HELLO_OK     | `0x02` | ←  | `[2B][ownId][2B][peerId][2B][iamToken]` |
| HELLO_ERR    | `0x03` | ←  | причина |
| PEER_CONN    | `0x04` | ↔  | `[2B][peerId][2B][iamToken]` |
| PEER_GONE    | `0x05` | ↔  | — |
| SYNC         | `0x06` | →  | — |
| PING / PONG  | `0xF0` / `0xF1` | ↔ | (PONG несёт обновлённый iamToken) |
| **PACKET**   | `0x10` | ↔  | один сырой IP-пакет |
| **PACKET_BATCH** | `0x11` | ↔ | повторяющиеся `[2B len][raw IP-пакет]` |

Никаких `streamID`, `seqID`, `OPEN`/`OPEN_OK`/`FIN`/`RST` больше нет. Потерянная датаграмма — это просто потерянный IP-пакет.

### Объединение пакетов (write coalescing)

`writeCoalescing` пакует IP-пакеты, пришедшие с TUN в течение `delayMs`, в один кадр `PACKET_BATCH`. Снижает накладные расходы WebSocket на болтливых нагрузках (мелкие TCP-сегменты / ACKи) ценой небольшой задержки. По умолчанию **выключено** — встроенного TCP-объединения обычно хватает.

### Режим relay

Если хелпер не может достучаться до gRPC `wsSend` (например, в ограниченной сети), установите `wsApi.relay: true`. Хелпер шлёт данные через upstream WS, Cloud Function ретранслирует их адаптеру. Обратный путь (adapter → helper) по-прежнему через `wsSend` напрямую. Медленнее и нестабильнее, но рабочее.

**Важно**: один адаптер — одна функция — один хелпер.

---

## Важное

Через сколько вас Яндекс за такое забанит — без понятия.

Рекомендации:

- Использовать только для самых важных сервисов с небольшим трафиком. Не злоупотребляйте.
- Перед загрузкой кода Cloud Function прогоните её JS-обфускатором, можно пару раз. Замените стандартные пути (`/_upstream`, `/_helper`, `/_conn-ids`) на свои.

### Скорость в реальной жизни

Это serverless WebSocket, который притворяется L3-туннелем. Чудес не ждите:

- **Веб-сёрфинг** — почти невыносимо. Загрузка страниц по ощущениям как 2G/EDGE: пара сотен кбит/с в пике, многосекундные TLS-хендшейки на крупных сайтах. Стриминги и большие загрузки — забудьте.
- **Telegram** — работает **удивительно хорошо**. Протокол Telegram гоняет мелкие сообщения, легко реконнектится и нечувствителен к задержкам — туннелю это как раз подходит. Чаты, голосовые сообщения, мелкие картинки/стикеры — всё юзабельно.
- **Рекомендуемая схема**: используйте **per-app tunneling** в Android-приложении и пустите через туннель **только Telegram** (плюс, при желании, одно-два других нетребовательных к трафику приложения). Остальное — мимо.

### Избежание петли

Хелперу нужен путь до YC API Gateway, который **не** идёт через сам туннель — иначе WebSocket пойдёт через себя же и ничего не заработает.

- **Go-хелпер** — обычный процесс. На Linux/macOS добавьте host-route для IP API Gateway через ваш реальный default gw: `ip route add <gw-ip>/32 via <real-gw>`. Хелпер логирует резолвленные IP при старте.
- **MAUI Android-приложение** через `VpnService.Builder.addDisallowedApplication(packageName)` исключает себя из своего же VPN — никакой ручной настройки не нужно.
- На Windows: `route add <gw-ip> <real-gw>`.

---

## Adapter

Ставится на VPS, в идеале в РФ поближе к Яндексу.

### Сборка

Нужен Go 1.21+ (toolchain автоматически возьмёт нужную для `wireguard/tun` версию).

```bash
cd adapter-and-helper
go build -o adapter ./cmd/adapter
```

На **Windows** положите рядом с бинарём `wintun.dll` под вашу архитектуру (<https://www.wintun.net/>). WireGuard работает так же.

### Конфигурация

`adapter.config.yaml`:

```yaml
bridge:
  url: "wss://<домен-api-gateway>/_adapter"
  authToken: "<общий-секрет>"
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

| Ключ | Описание |
|------|----------|
| `bridge.url` | WebSocket URL эндпоинта API Gateway для адаптера |
| `bridge.authToken` | Общий секрет (должен совпадать с `AUTH_TOKEN` в Cloud Function) |
| `bridge.reconnect` | Экспоненциальный backoff для переподключения upstream |
| `bridge.pingIntervalMs` | Интервал PING (держите менее 10 мин) |
| `tun.name` | Имя интерфейса. Linux: `btf0`. Windows: имя wintun-адаптера. macOS: оставьте пустым (utun сам назначит). |
| `tun.address` | Локальный IP туннеля, например `10.200.0.1`. С `peerAddress` образует `/30`. |
| `tun.peerAddress` | IP пира, например `10.200.0.2`. |
| `tun.mtu` | MTU. 1400 — безопасный дефолт с запасом на WS+TLS+IP-overhead. |
| `http.listenPort` | HTTP-порт для `/conn-ids` (используется Cloud Function при холодном старте) |
| `writeCoalescing.enabled` | Паковать несколько IP-пакетов в один `PACKET_BATCH`. Снижает rate `wsSend`. |
| `writeCoalescing.delayMs` | Окно батчинга (мс). Вынужденный flush при 32 КиБ. Если включено, рекомендуется 1–10 мс. |
| `wsApi.mode` | `grpc` (единственный вариант) |

### IP-форвардинг на сервере (Linux)

Адаптер только пишет пакеты в свой TUN — он **не** делает NAT и не форвардит их сам. Чтобы трафик хелпера реально доходил до интернета:

```bash
sudo sysctl -w net.ipv4.ip_forward=1

# MASQUERADE трафика хелпера за egress IP сервера. Замените eth0 и сеть /30 при необходимости.
sudo iptables -t nat -A POSTROUTING -s 10.200.0.0/30 -o eth0 -j MASQUERADE
sudo iptables -A FORWARD -i btf0 -o eth0 -j ACCEPT
sudo iptables -A FORWARD -i eth0 -o btf0 -m state --state RELATED,ESTABLISHED -j ACCEPT
```

Сохраните через `iptables-persistent` / `nftables` / средства вашего дистрибутива.

Если нужно отдавать только конкретные сервисы — форвардинг можно не включать, а сервисы биндить прямо на туннельный IP `10.200.0.1`.

### Запуск

```bash
sudo ./adapter adapter.config.yaml
```

Нужны root или `CAP_NET_ADMIN` (для создания TUN).

### HTTP-эндпоинты

| Эндпоинт | Метод | Описание |
|-----------|-------|----------|
| `/conn-ids` | GET | Возвращает `{"adapterConnId":"...","helperConnId":"..."}`. Авторизация: `Bearer <authToken>`. |

---

## Helper (Go)

Ставится на клиентское устройство. Также есть MAUI **Android**-приложение — см. [maui-client/README.md](maui-client/README.md).

### Сборка

```bash
cd adapter-and-helper
go build -o helper ./cmd/helper
```

На Windows положите рядом `wintun.dll`.

### Конфигурация

```yaml
bridge:
  url: "wss://<домен-api-gateway>/_helper"
  authToken: "<общий-секрет>"
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

| Ключ | Описание |
|------|----------|
| `tun.address` / `tun.peerAddress` | Зеркальны для адаптера: local хелпера = peer адаптера и наоборот. |
| `wsApi.relay` | `true` = через upstream WS (Cloud Function ретранслирует). `false` = напрямую `wsSend`. |

### Запуск

```bash
sudo ./helper helper.config.yaml
```

### Маршрутизация на клиенте

Хелпер открывает TUN, но **не** прописывает default route. Решите, что хотите гнать через туннель, и добавьте маршруты сами. Два частых варианта:

**A. Всё через туннель** (full VPN). Сначала запиньте IP API Gateway на реальный gw, чтобы избежать петли:

```bash
GW_IP=$(getent hosts <api-gateway-domain> | awk '{print $1}' | head -n1)
REAL_GW=$(ip route | awk '/default/ {print $3; exit}')
REAL_DEV=$(ip route | awk '/default/ {print $5; exit}')

sudo ip route add ${GW_IP}/32 via ${REAL_GW} dev ${REAL_DEV}

sudo ip route add 0.0.0.0/1 via 10.200.0.1 dev btf0
sudo ip route add 128.0.0.0/1 via 10.200.0.1 dev btf0
```

**B. Только конкретные подсети**:

```bash
sudo ip route add 203.0.113.0/24 via 10.200.0.1 dev btf0
```

DNS отдельной настройки не требует — поправьте резолвер в `/etc/resolv.conf` (например, `1.1.1.1`), и он будет работать через туннель.

---

## Развёртывание Cloud Function (YC)

Cloud Function **знает** о протоколе: парсит HELLO, проверяет токен, обменивает идентификаторы соединений и (в режиме relay) пересылает кадры `PACKET`/`PACKET_BATCH` между сторонами. При апгрейде Go-бинарей до IP-туннеля обязательно передеплойте `bridge-cloud/index.js` — иначе хендшейк сломается (старая v4-функция ждёт 9-байтный stream-frame header, которого v5 уже не шлёт).

### Предварительные требования

- Аккаунт YC с биллингом
- `yc` CLI (`yc init`)
- Каталог в облаке

### 1. Сервисный аккаунт

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

### 2. Cloud Function

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
  --environment "AUTH_TOKEN=<ваш-секрет>,ADAPTER_URL=<http-url-адаптера>"
```

### 3. API Gateway

Отредактируйте `bridge-cloud/spec.yaml` (`${FUNCTION_ID}` и `${SERVICE_ACCOUNT_ID}`):

```bash
yc serverless api-gateway create --name bridge-gw --spec spec.yaml
GW_DOMAIN=$(yc serverless api-gateway get bridge-gw --format json | jq -r .domain)
echo "Gateway: wss://$GW_DOMAIN"
```

### 4. Запуск адаптера и хелпера

Пропишите соответствующие `bridge.url` в обоих конфигах и запустите как описано выше.

### 5. Использование

Никакого клиентского порта больше нет — приложения используют туннель на IP-уровне через ваши маршруты.

Проверка:

```bash
ping 10.200.0.1   # с хелпера — пингует адаптер
```

### 6. Логи

```bash
yc logging read --folder-id $(yc config get folder-id) --follow
```

---

## Лимиты YC

| Лимит | Значение |
|-------|----------|
| Максимальное время жизни WebSocket | 60 минут |
| Idle timeout | 10 минут |
| Максимальный размер сообщения | 128 КБ |
| Максимальный размер фрейма | 32 КБ |
| Таймаут выполнения функции | 10 с |

`pingIntervalMs` ставьте меньше 10 мин. 128 КБ — потолок одного `PACKET_BATCH`-сообщения.

## Лицензия

WTFPL, см. [LICENSE](LICENSE).
Дальнейших доработок не будет, поддержки и ответов на вопросы — тоже. Goodbye and kiss my ass.
