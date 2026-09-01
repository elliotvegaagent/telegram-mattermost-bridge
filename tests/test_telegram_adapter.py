import json

import aiohttp
import pytest
from aiohttp import web

from bridge.adapters.telegram import TelegramAdapter
from bridge.models import BridgeEvent, DeliveryContext, EventKind, Platform


async def start_app(app: web.Application) -> tuple[web.AppRunner, str]:
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0)
    await site.start()
    port = site._server.sockets[0].getsockname()[1]
    return runner, f"http://127.0.0.1:{port}"


@pytest.mark.asyncio
async def test_groups_album_and_filters_foreign_chat() -> None:
    async with aiohttp.ClientSession() as session:
        adapter = TelegramAdapter(session, token="secret", chat_id=-100)
        updates = [
            {
                "update_id": 1,
                "message": {
                    "message_id": 10,
                    "date": 1,
                    "media_group_id": "album",
                    "chat": {"id": -100},
                    "from": {"id": 5, "first_name": "A"},
                    "caption": "files",
                    "photo": [{"file_id": "f1", "file_unique_id": "u1", "file_size": 4}],
                },
            },
            {
                "update_id": 2,
                "message": {
                    "message_id": 11,
                    "date": 1,
                    "media_group_id": "album",
                    "chat": {"id": -100},
                    "from": {"id": 5, "first_name": "A"},
                    "photo": [{"file_id": "f2", "file_unique_id": "u2", "file_size": 5}],
                },
            },
            {
                "update_id": 3,
                "message": {
                    "message_id": 12,
                    "date": 1,
                    "chat": {"id": -200},
                    "from": {"id": 6, "first_name": "B"},
                    "text": "foreign",
                },
            },
        ]
        events = adapter.events_from_updates(updates, self_user_id="99")
    assert len(events) == 2
    album = events[0]
    assert album.message_ids == ("10", "11")
    assert len(album.attachments) == 2
    assert album.checkpoint_value == "3"
    assert events[1].kind is EventKind.IGNORED
    assert events[1].checkpoint_value == "4"


@pytest.mark.asyncio
async def test_routes_updates_from_multiple_telegram_chats() -> None:
    async with aiohttp.ClientSession() as session:
        adapter = TelegramAdapter(
            session,
            token="secret",
            routes={"a": -100, "b": -200},
        )
        updates = [
            {
                "update_id": 10,
                "message": {
                    "message_id": 20,
                    "date": 1,
                    "chat": {"id": -100},
                    "from": {"id": 5, "first_name": "A"},
                    "text": "from a",
                },
            },
            {
                "update_id": 11,
                "message": {
                    "message_id": 21,
                    "date": 1,
                    "chat": {"id": -200},
                    "from": {"id": 6, "first_name": "B"},
                    "text": "from b",
                },
            },
        ]

        events = adapter.events_from_updates(updates, self_user_id="99")

    assert [(event.route_id, event.text) for event in events] == [
        ("a", "from a"),
        ("b", "from b"),
    ]


@pytest.mark.asyncio
async def test_polling_explicitly_subscribes_to_reaction_updates() -> None:
    allowed_updates: list[str] = []

    async def get_updates(request: web.Request) -> web.Response:
        data = await request.post()
        allowed_updates.extend(json.loads(data["allowed_updates"]))
        return web.json_response({"ok": True, "result": []})

    app = web.Application()
    app.router.add_post("/botsecret/getUpdates", get_updates)
    runner, base = await start_app(app)
    try:
        async with aiohttp.ClientSession() as session:
            adapter = TelegramAdapter(
                session, token="secret", chat_id=-100, api_base=base
            )
            await adapter.get_updates(0)
    finally:
        await runner.cleanup()

    assert allowed_updates == ["message", "edited_message", "message_reaction"]


@pytest.mark.asyncio
async def test_sends_mattermost_message_to_routed_telegram_chat() -> None:
    chat_ids: list[str] = []

    async def send_message(request: web.Request) -> web.Response:
        data = await request.post()
        chat_ids.append(data["chat_id"])
        return web.json_response({"ok": True, "result": {"message_id": 77}})

    app = web.Application()
    app.router.add_post("/botsecret/sendMessage", send_message)
    runner, base = await start_app(app)
    event = BridgeEvent(
        event_id="mm:post-b:message",
        platform=Platform.MATTERMOST,
        kind=EventKind.MESSAGE,
        message_id="post-b",
        message_ids=("post-b",),
        author_id="employee",
        author_name="Employee",
        route_id="b",
        text="hello",
    )

    async def on_sent(_) -> None:
        return None

    try:
        async with aiohttp.ClientSession() as session:
            adapter = TelegramAdapter(
                session,
                token="secret",
                routes={"a": -100, "b": -200},
                api_base=base,
            )
            await adapter.send(
                event,
                DeliveryContext(reply_to_id=None, root_id=None, anchor_id=None),
                [],
                [],
                0,
                on_sent,
            )
    finally:
        await runner.cleanup()
    assert chat_ids == ["-200"]


@pytest.mark.asyncio
async def test_ignores_service_message_and_labels_anonymous_admin() -> None:
    async with aiohttp.ClientSession() as session:
        adapter = TelegramAdapter(session, token="secret", chat_id=-100)
        updates = [
            {
                "update_id": 4,
                "message": {
                    "message_id": 13,
                    "date": 1,
                    "chat": {"id": -100},
                    "from": {"id": 5, "first_name": "A"},
                    "new_chat_members": [{"id": 7}],
                },
            },
            {
                "update_id": 5,
                "message": {
                    "message_id": 14,
                    "date": 1,
                    "chat": {"id": -100},
                    "sender_chat": {"id": -100, "title": "Contractor Group"},
                    "text": "anonymous",
                },
            },
        ]
        events = adapter.events_from_updates(updates, self_user_id="99")
    assert events[0].kind is EventKind.IGNORED
    assert events[1].author_name == "Contractor Group"


@pytest.mark.asyncio
async def test_delivery_acknowledgement_does_not_occupy_telegram_reaction() -> None:
    received: list[tuple[str, str]] = []

    async def set_reaction(request: web.Request) -> web.Response:
        data = await request.post()
        received.append((data["message_id"], data["reaction"]))
        return web.json_response({"ok": True, "result": True})

    app = web.Application()
    app.router.add_post("/botsecret/setMessageReaction", set_reaction)
    runner, base = await start_app(app)
    event = BridgeEvent(
        event_id="tg:album",
        platform=Platform.TELEGRAM,
        kind=EventKind.MESSAGE,
        message_id="10",
        message_ids=("10", "11"),
        author_id="20",
        author_name="Contractor",
    )
    try:
        async with aiohttp.ClientSession() as session:
            adapter = TelegramAdapter(session, token="secret", chat_id=-100, api_base=base)
            await adapter.acknowledge_delivery(event)
    finally:
        await runner.cleanup()
    assert received == []


@pytest.mark.asyncio
async def test_normalizes_basic_telegram_reaction_replacement() -> None:
    async with aiohttp.ClientSession() as session:
        adapter = TelegramAdapter(session, token="secret", chat_id=-100)
        events = adapter.events_from_updates(
            [
                {
                    "update_id": 15,
                    "message_reaction": {
                        "chat": {"id": -100},
                        "message_id": 25,
                        "user": {"id": 5, "first_name": "A"},
                        "date": 123,
                        "old_reaction": [{"type": "emoji", "emoji": "👍"}],
                        "new_reaction": [{"type": "emoji", "emoji": "❤️"}],
                    },
                }
            ],
            self_user_id="99",
        )

    assert [(event.kind, event.reaction) for event in events] == [
        (EventKind.REACTION_REMOVED, "👍"),
        (EventKind.REACTION_ADDED, "❤️"),
    ]
    assert events[0].checkpoint_value is None
    assert events[1].checkpoint_value == "16"
    assert all(event.message_id == "25" for event in events)


@pytest.mark.asyncio
async def test_ignores_custom_and_own_telegram_reactions_but_advances_offset() -> None:
    async with aiohttp.ClientSession() as session:
        adapter = TelegramAdapter(session, token="secret", chat_id=-100)
        events = adapter.events_from_updates(
            [
                {
                    "update_id": 16,
                    "message_reaction": {
                        "chat": {"id": -100},
                        "message_id": 26,
                        "user": {"id": 99, "first_name": "Bridge"},
                        "date": 124,
                        "old_reaction": [],
                        "new_reaction": [
                            {"type": "custom_emoji", "custom_emoji_id": "custom"}
                        ],
                    },
                }
            ],
            self_user_id="99",
        )

    assert len(events) == 1
    assert events[0].kind is EventKind.IGNORED
    assert events[0].checkpoint_value == "17"


@pytest.mark.asyncio
async def test_sets_at_most_one_telegram_reaction() -> None:
    received: list[dict[str, str]] = []

    async def set_reaction(request: web.Request) -> web.Response:
        received.append(dict(await request.post()))
        return web.json_response({"ok": True, "result": True})

    app = web.Application()
    app.router.add_post("/botsecret/setMessageReaction", set_reaction)
    runner, base = await start_app(app)
    event = BridgeEvent(
        event_id="mm:reaction",
        platform=Platform.MATTERMOST,
        kind=EventKind.REACTION_ADDED,
        message_id="post1",
        message_ids=("post1",),
        author_id="employee",
        author_name="Employee",
        reaction="❤️",
    )
    try:
        async with aiohttp.ClientSession() as session:
            adapter = TelegramAdapter(session, token="secret", chat_id=-100, api_base=base)
            await adapter.sync_reactions(event, "77", ("👍", "❤️"))
            await adapter.sync_reactions(event, "77", ())
    finally:
        await runner.cleanup()

    assert received == [
        {
            "chat_id": "-100",
            "message_id": "77",
            "reaction": '[{"type":"emoji","emoji":"❤️"}]',
        },
        {"chat_id": "-100", "message_id": "77", "reaction": "[]"},
    ]


@pytest.mark.asyncio
async def test_edits_linked_telegram_message_without_correction_notice() -> None:
    edits: list[dict[str, str]] = []

    async def edit_message(request: web.Request) -> web.Response:
        data = await request.post()
        edits.append(dict(data))
        return web.json_response(
            {"ok": True, "result": {"message_id": int(data["message_id"])}}
        )

    app = web.Application()
    app.router.add_post("/botsecret/editMessageText", edit_message)
    runner, base = await start_app(app)
    event = BridgeEvent(
        event_id="mm:post1:edit:2",
        platform=Platform.MATTERMOST,
        kind=EventKind.EDIT,
        message_id="post1",
        message_ids=("post1",),
        author_id="employee",
        author_name="Дмитрий",
        text="Спасибо",
    )
    try:
        async with aiohttp.ClientSession() as session:
            adapter = TelegramAdapter(session, token="secret", chat_id=-100, api_base=base)
            await adapter.edit(event, "77")
    finally:
        await runner.cleanup()
    assert edits == [
        {"chat_id": "-100", "message_id": "77", "text": "Дмитрий : Спасибо"}
    ]


@pytest.mark.asyncio
async def test_fetches_small_telegram_profile_photo() -> None:
    async def profile_photos(_: web.Request) -> web.Response:
        return web.json_response(
            {
                "ok": True,
                "result": {
                    "total_count": 1,
                    "photos": [
                        [
                            {
                                "file_id": "small-avatar",
                                "file_unique_id": "avatar-20",
                                "width": 160,
                                "height": 160,
                                "file_size": 4,
                            },
                            {
                                "file_id": "large-avatar",
                                "file_unique_id": "avatar-20-large",
                                "width": 640,
                                "height": 640,
                                "file_size": 100,
                            },
                        ]
                    ],
                },
            }
        )

    async def get_file(_: web.Request) -> web.Response:
        return web.json_response(
            {"ok": True, "result": {"file_path": "avatars/20.jpg", "file_size": 4}}
        )

    async def avatar_file(_: web.Request) -> web.Response:
        return web.Response(body=b"JPEG", content_type="image/jpeg")

    app = web.Application()
    app.router.add_post("/botsecret/getUserProfilePhotos", profile_photos)
    app.router.add_post("/botsecret/getFile", get_file)
    app.router.add_get("/file/botsecret/avatars/20.jpg", avatar_file)
    runner, base = await start_app(app)
    event = BridgeEvent(
        event_id="tg:10",
        platform=Platform.TELEGRAM,
        kind=EventKind.MESSAGE,
        message_id="10",
        message_ids=("10",),
        author_id="20",
        author_name="Contractor",
    )
    try:
        async with aiohttp.ClientSession() as session:
            adapter = TelegramAdapter(session, token="secret", chat_id=-100, api_base=base)
            avatar = await adapter.fetch_author_avatar(event, max_bytes=1024)
    finally:
        await runner.cleanup()
    assert avatar is not None
    assert (avatar.file_name, avatar.mime_type, avatar.data) == (
        "telegram_avatar_20.jpg",
        "image/jpeg",
        b"JPEG",
    )
