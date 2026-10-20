# ai-sandbox

Флот одноразовых Linux-песочниц для AI-агентов: агент получает MCP-сервер с
командой `shell` под root, работает, отдаёт файлы и отпускает машину — она
архивируется в Google Drive, очищается и уходит следующему.

```
Telegram-бот / ChatGPT / любой MCP-клиент
            │ HTTPS
      control box (пул)                 ← Vibe Hosting / любой VPS / локально
            ├── rclone → gdrive:ai-sandbox/<owner>/<sandbox>/
            └── SSH/API → N песочниц (по одной на задачу)
```

## Два бинаря

| Бинарь | Где живёт | Что делает |
|---|---|---|
| `sandboxd` | на каждой песочнице | MCP-сервер: tools, OAuth, `/health`, стейт-машина, отправка архива в пул |
| `sandbox-pool` | control box | Аккаунты, очередь, lease, provisioning, health-пингер, приём архивов, квота GDrive, уведомления |

## Песочница (`sandboxd`)

* Транспорт: MCP Streamable HTTP, `POST /mcp`, JSON-RPC 2.0.
* Авторизация: статический `SANDBOX_TOKEN` как `Authorization: Bearer …`
  плюс мини-OAuth (RFC 9728 + 8414) для веб-коннекторов:
  discovery → DCR → consent (вводится тот же токен) → одноразовый code с PKCE
  S256 → token endpoint. Клиенты и code живут в памяти, code одноразовый.
* `/health` — публичный JSON: `state` (`free`/`busy`), `phase`
  (`idle`/`archiving`/`archive_error`), `idle_seconds`, `free_in_seconds`,
  `active_calls`, размер воркспейса, свободный диск, состояние архива.
* `/f/<token>` — временная подписанная ссылка на файл (15 минут).

### Tools

| Tool | Что делает |
|---|---|
| `read` | Текстовый файл из `/workspace` (offset, limit, base64 для бинарных) |
| `write` | Атомарная запись текста, создаёт родителей |
| `upload` | Отдаёт файл наружу: `mode=raw` — байты inline как MCP resource, `mode=url` — подписанная ссылка |
| `delete` | Файл или каталог (`recursive`) |
| `shell` | `/bin/bash -c` под root, свой process group, таймаут, лимит вывода |
| `execute` | Полный alias `shell` |
| `end_work` | Архив → очистка → освобождение машины |

Файловые tools заперты в `/workspace`: `..`, абсолютные пути наружу и symlink-компоненты
запрещены. В shell уходит чистый env (без `SANDBOX_TOKEN`, `ARCHIVE_TOKEN` и
кредов rclone).

### Жизненный цикл

```
free ──(любой MCP-запрос)──> busy ──(15 мин тишины | end_work)──> archiving ──> free
```

* Таймер считает **не MCP-запросы**: `/health` его не сбрасывает, поэтому
  health-пинг контроллера будит контейнер, но не мешает освобождению.
* `archiving` — архив летит в пул; файлы удаляются **только** после
  подтверждения от пула. Неудача → `archive_error`, повтор через
  `ARCHIVE_RETRY`, файлы на месте.
* Стейт в `STATE_FILE` (вне воркспейса) — переживает рестарт сервиса.

## Пул (`sandbox-pool`)

* Аккаунты: `api_key` провайдера, лимиты, кредиты.
* Очередь: `POST /v1/acquire` (long-poll до `ACQUIRE_WAIT`) → если машин нет,
  запрос встаёт в очередь и пул поднимает новую машину.
* Lease: машина принадлежит одному пользователю до `end_work`.
* Health-пингер: раз в `HEALTH_INTERVAL` будит занятые машины; если машина
  сама освободилась — пул её удаляет и отдаёт следующему из очереди.
* Архивы: `POST /v1/archive` (Bearer = `ARCHIVE_TOKEN` песочницы) → стрим в
  `rclone rcat` → `gdrive:ai-sandbox/<owner>/<sandbox>/<ts>_<reason>.tar.gz`
  + `.json` с метаданными (sha256, размер, причина).
* Квота: `ARCHIVE_QUOTA_BYTES` (10 ГиБ) на пользователя; на 80% — предупреждение,
  при переполнении — удаление самого старого архива + уведомление.
* Уведомления: Telegram (`TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHAT_ID`) и/или
  `GET /v1/events` для бота.

### API

| Метод | Путь | Что делает |
|---|---|---|
| `GET` | `/health` | статус пула (публично) |
| `POST` | `/v1/acquire` | арендовать песочницу (или встать в очередь) |
| `POST` | `/v1/release` | освободить lease |
| `GET` | `/v1/status` | аккаунты, машины, очередь, lease |
| `GET` | `/v1/lease/{id}` | состояние lease |
| `GET` | `/v1/events?after=N` | события (предупреждения, архивы) |
| `POST` | `/v1/machines` | добавить машину вручную (ssh-провайдер) |
| `POST` | `/v1/accounts` | добавить аккаунт провайдера |
| `POST` | `/v1/archive` | приём архива от песочницы |

Кроме `POOL_TOKEN` (Bearer) все `/v1/*` требуют авторизации, `/health` — нет.

## Провайдеры машин

`POOL_PROVIDER=vibe|ssh|local`

* **vibe** — машины создаются через API Vibe Hosting (`POST /create`), ставятся
  по SSH, удаляются после освобождения.
* **ssh** — машины добавляются вручную (`POST /v1/machines`: host, port,
  private key, url). Подойдёт **любой VPS с root SSH**. Пул только деплоит
  песочницу и управляет lease, но не создаёт и не удаляет машины.
* **local** — песочницы запускаются процессами на самом control box. Режим для
  разработки и тестов (`go test ./internal/pool -run LocalProvider`).

## Сборка и деплой

```sh
make build          # локальная сборка
make release        # dist/sandboxd-linux-{amd64,arm64}, dist/sandbox-pool-linux-amd64
make test
```

Control box (Vibe Hosting или свой VPS):

```sh
scripts/deploy_pool.sh <host> <port> <ssh-key> https://control.example.com
```

Скрипт кладёт бинари, пишет `/root/pool.env` и `/root/run.sh` (Vibe сам
переключает `:80` на твой процесс). Токен пула пишется в
`~/secrets/ai-sandbox-vibe/pool.env` (0600).

## GDrive

Пул гоняет `rclone` (CLI, без SDK):

```sh
rclone config                       # remote с именем gdrive
rclone lsd gdrive:ai-sandbox
```

Креды лежат только на control box (`RCLONE_CONFIG`), в песочницы не попадают.
Квота на пользователя считается пулом по папке `<owner>`.

## Ограничения

* Shell работает под root: граница доверия — сама машина. После освобождения
  её не переиспользуют под другого пользователя, а удаляют.
* Диск 1 ГБ на машину (лимит провайдера); пул держит резерв
  `MIN_FREE_DISK_MB` и отказывает в записи, когда места мало.
* Архивы — это tar.gz воркспейса; системные изменения машины не сохраняются.

## Telegram-бот

`projects/tgcloud-bot/tgcloud/lib/sandbox.js` — клиент пула: арендует песочницу
на чат, хранит lease в БД, зовёт MCP-тулы. Tools агента: `sandbox_shell`,
`sandbox_read`, `sandbox_write`, `sandbox_upload`, `sandbox_file`,
`sandbox_delete`, `sandbox_end`, `sandbox_status`.

Нужны секрет `POOL_TOKEN` и миграция таблицы `sandboxes`.
