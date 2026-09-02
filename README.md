# Telegram ↔ Mattermost Bridge на Go

Самостоятельная Go-реализация двустороннего моста между одной или несколькими
парами «Telegram-группа ↔ приватный Mattermost-канал». Проект совместим со схемой
SQLite Python-версии и может продолжить работу с её offset, checkpoint, связями
тредов и состоянием реакций.

```text
Telegram long polling ─┐
                      ├→ Gateway.Accept(event) → SQLite inbox/outbox → delivery worker
Mattermost WebSocket ──┘                         ↕ message/thread/reaction links
```

`Gateway` — глубокий модуль с одним входным интерфейсом `Accept`. Адаптеры только
нормализуют внешние события и выполняют доставку; дедупликация, retry, durable
outbox, треды, edit и реакции находятся внутри Gateway.

## Возможности

- самостоятельное сообщение создаёт новый диалог, Reply сохраняет тред;
- формат авторов: `Антон: Спасибо` в Mattermost; в Telegram имя автора выводится
  отдельной первой строкой курсивом, а основной текст — со следующей строки;
- текст, фото, document, video, audio, voice, animation и Telegram-альбомы;
- несколько Mattermost-файлов отправляются последовательно, не более пяти на пост;
- безопасное экранирование Mattermost Markdown и всех внешних `@mention`;
- бесшумное редактирование уже связанного текстового сообщения;
- уведомление об удалении Mattermost-поста;
- реакции `👍`, `👎`, `❤️`, `🔥`, `🎉`, `✅`, `👀`, `😂` в обе стороны;
- имя и аватар Telegram-автора через Mattermost override, если разрешено сервером;
- несколько независимых пар через `BRIDGE_PAIRS`;
- SQLite WAL, durable inbox/outbox, дедупликация и dead letter;
- retry для сетевых ошибок, `429` и `5xx`, с `Retry-After` и backoff до 5 минут;
- REST-восстановление Mattermost после WebSocket-разрыва с перекрытием 5 минут;
- JSON-логи без токенов и текста сообщений;
- `/healthz` и `/readyz`, degraded при недоступном адаптере или очереди старше 10 минут.

Telegram разрешает боту поставить только одну реакцию на сообщение, поэтому там
показывается последняя активная реакция. В Mattermost одновременно сохраняются
все поддерживаемые реакции; технически они принадлежат bridge-боту.

## Конфигурация

```bash
cp .env.example .env
chmod 600 .env
```

Обязательные переменные:

| Переменная | Назначение |
|---|---|
| `MM_URL` | HTTPS URL Mattermost |
| `MM_BOT_TOKEN` | token отдельного Mattermost bot account |
| `MM_CHANNEL_ID` | канал Mattermost для одной пары |
| `TG_BOT_TOKEN` | token Telegram-бота |
| `TG_CHAT_ID` | Telegram group/supergroup ID для одной пары |
| `DB_PATH` | SQLite, в Compose `/data/bridge.db` |

Для нескольких пар `BRIDGE_PAIRS` заменяет `MM_CHANNEL_ID` и `TG_CHAT_ID`:

```env
BRIDGE_PAIRS=[{"id":"default","tg_chat_id":-1001111111111,"mm_channel_id":"channel_a"},{"id":"customer-b","tg_chat_id":-1002222222222,"mm_channel_id":"channel_b"}]
```

Для существующей пары сохраняйте ID `default`, чтобы использовать её checkpoint.

Через `@BotFather` отключите Privacy Mode и заново добавьте бота в группу либо
сделайте его администратором. Для событий реакций бот должен быть администратором.
Mattermost bot account должен состоять только в нужной команде и каналах.

## Запуск

Проверка токенов, ID, членства и записи SQLite:

```bash
docker compose build
docker compose run --rm bridge doctor
```

Первый запуск новой базы намеренно удаляет накопленные Telegram updates и ставит
Mattermost checkpoint «сейчас»:

```bash
docker compose run --rm bridge init
docker compose up -d
```

Повторный `init` запрещён; осознанный сброс checkpoint выполняется `init --force`.

```bash
curl --fail http://127.0.0.1:8080/healthz
curl --fail http://127.0.0.1:8080/readyz
docker compose logs -f bridge
```

После запуска можно отправить по одному явно помеченному тестовому сообщению в
каждую сторону через durable pipeline:

```bash
docker compose run --rm bridge smoke --confirm
```

Без Docker:

```bash
go run ./cmd/bridge doctor
go run ./cmd/bridge init
go run ./cmd/bridge run
```

## Переход с Python-версии

Go и Python используют одинаковые миграции 1–2. Два процесса нельзя запускать
одновременно с одним Telegram token или одним SQLite-файлом.

1. Убедитесь, что `/readyz` Python-версии показывает пустую активную очередь.
2. Остановите Python-контейнер и сделайте резервную копию `bridge.db` вместе с WAL/SHM.
3. Скопируйте `.env` без вывода секретов.
4. Для использования прежнего Docker volume запускайте production override:

   ```bash
   docker compose -f compose.yaml -f compose.production.yaml build
   docker compose -f compose.yaml -f compose.production.yaml run --rm bridge doctor
   docker compose -f compose.yaml -f compose.production.yaml up -d
   ```

`init` при миграции выполнять не нужно. Для отката остановите Go Compose и снова
запустите прежний Python Compose: схема базы и связи остаются совместимыми.

## Разработка и проверка

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/bridge
```

Тесты используют временные SQLite и локальные fake HTTP/WebSocket endpoints; реальные
секреты не требуются.

## Гарантии доставки

Событие и checkpoint фиксируются атомарно до продвижения Telegram offset или
Mattermost checkpoint. После успешной доставки payload удаляется из SQLite, а ID,
статус и связи остаются. Семантика at-least-once: авария строго между внешней
отправкой и записью результата теоретически может создать один дубль.

Редактирование файла, custom/paid emoji, интерактивные опросы, синхронное удаление
чужих Telegram-сообщений и восстановление реакций Mattermost из истории не входят
в текущую версию.
