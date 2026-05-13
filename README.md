# product-price-parser

[![CI](https://github.com/XaCaMaCa/product-price-parser/actions/workflows/ci.yml/badge.svg)](https://github.com/XaCaMaCa/product-price-parser/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/badge/go-1.22-blue.svg)](https://go.dev/)

Тестовое на Go-разработчика: консольная утилита, которая тащит **lenta.com** не через селекторы по HTML, а через **JSON с их API** — так проще пережить смену вёрстки. В ТЗ ещё были прокси, выбор магазина, категории; с Qratorом пришлось возиться: куки, нормальные заголовки, при необходимости uTLS (ClientHello как у Chrome).

По файлам: `main.go` — флаги, разбор JSON в товары, JSONL; `lenta.go` — запросы к API, магазин; `client.go` — HTTP, редиректы `www`?apex; `localproxy.go` и `utls_roundtripper.go` — прокси и TLS.

### Архитектура

```mermaid
flowchart LR
    CLI[main.go<br/>CLI flags] --> Lenta[lenta.go<br/>Lenta scraper]
    Lenta --> Client[client.go<br/>http.Client]
    Client --> RT{Transport}
    RT -->|chrome-tls| UTLS[utls_roundtripper.go<br/>uTLS Chrome ClientHello]
    RT -->|standard| Default[http.DefaultTransport]
    UTLS --> Proxy[localproxy.go<br/>CONNECT or direct]
    UTLS --> Target[lenta.com<br/>JSON API]
    Default --> Target
    Lenta --> Out[products.jsonl]
```

### Поток обхода защиты

```mermaid
sequenceDiagram
    participant CLI
    participant Lenta
    participant UTLS as utlsProxyRT
    participant Proxy
    participant Target as lenta.com

    CLI->>Lenta: запрос категорий
    Lenta->>UTLS: GET /api/...
    UTLS->>Proxy: CONNECT host:443
    Proxy-->>UTLS: 200 Connection Established
    UTLS->>Target: TLS ClientHello (Chrome fingerprint)
    Target-->>UTLS: TLS handshake OK
    UTLS->>Target: HTTPS GET /api/...
    Target-->>UTLS: JSON
    UTLS-->>Lenta: response
    Lenta->>Lenta: parse + dedup
    Lenta-->>CLI: write to products.jsonl
```

## Сборка

```bash
go build -o parser.exe .
```

## Прокси и сеть

- **Онлайн:** укажите **`-proxy`** или **`-local-proxy`** (или **`-offline`**).
- **`-offline`:** сети нет, не нужен прокси; не совмещайте с **`-local-proxy`** и **`-chrome-tls`**.

### Внешний и локальный прокси

- **`-proxy http://user:pass@host:port`** — весь трафик к lenta.com идёт через **внешний** прокси (ротация IP, корпоративный шлюз, требование ТЗ «сбор через прокси» в буквальном смысле).
- **`-local-proxy`** — поднимается **локальный** HTTP-прокси на машине: клиент использует `Proxy` как положено, запросы уходят **напрямую** с вашего хоста, без внешнего апстрима. Удобно отлаживать `CONNECT` и uTLS, но **анонимизация IP не задумывалась**.
- В сочетании **`-local-proxy` + `-chrome-tls`** на loopback-прокси TLS к цели может **обходить CONNECT** (прямой TCP + uTLS — см. предупреждение в stderr), чтобы на части систем не было таймаута при dial на `127.0.0.1`. Для **продакшн-сбора** при необходимости смены исходящего адреса лучше **внешний** `-proxy`.

## Категории товаров

Список **не обходится** как дерево API «категория ? витрина» (контракт дольше живёт, если не ходить по каждой ветке отдельно). Беру **витринные** JSON (`/home`, промо и т.п.), потом **постфильтр** `-categories`: по подстроке в **категории/названии** и алиасам (молочка, хлеб, `milk`/`bread`). **`-per-category`** режет лимит по **строке категории из JSON**, а не по списку из флага `-categories`.

## Qrator (cookie, uTLS)

```powershell
.\parser.exe -local-proxy -chrome-tls -cookie-file .\cookie.txt -warmup-with-cookie -city "Москва" -out products.jsonl
```

`cookie.txt` — одна строка, UTF-8. В репозиторий не коммитить (см. `.gitignore`).

## Сборка без сети (fixtures)

С полем `category`:

```powershell
.\parser.exe -offline -categories "молочная продукция,хлебобулочные изделия" -out sample-export.jsonl
```

Только **наименование, цена, ссылка** (`-minimal-jsonl`):

```powershell
.\parser.exe -offline -categories "молочная продукция,хлебобулочные изделия" -minimal-jsonl -out export-dve-kategorii.jsonl
```

Артефакт: **`export-dve-kategorii.jsonl`**.

**URL в fixtures** вида `https://lenta.com/product/moloko-32` **демонстрационные** (в выгрузке). На живом lenta.com такие слуги устаревшие; в браузере часто **404** — нормальный рабочий путь: длинные slug+id, данные с **JSON API**; у Qrator 401/404 в онлайне бывает.

## Формат строк (JSON Lines)

- Полный: `category`, `name`, `price`, `url`.
- **`-minimal-jsonl`:** только `name`, `price`, `url`.

## Флаги

| Флаг | Назначение |
|------|------------|
| `-city`, `-store-id` | Город / магазин |
| `-categories` | Категории (подстроки) |
| `-per-category` | Лимит товаров на категорию; `0` = без лимита |
| `-minimal-jsonl` | В файле только name, price, url |
| `-cookie-file` / `-cookie` | Cookie |
| `-chrome-tls` | uTLS (ClientHello как у Chrome) |
| `-warmup-with-cookie` | Витрина + merge Set-Cookie |
| `-api-referer` | Referer из DevTools |
| `-omniweb-headers` | Доп. omniweb-заголовки |
| `-fixtures` | Каталог JSON при `-offline` |
