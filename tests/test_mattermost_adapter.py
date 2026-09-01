import aiohttp
import pytest
from aiohttp import web

from bridge.adapters.mattermost import MattermostAdapter
from bridge.models import (
    BridgeEvent,
    DeliveryContext,
    EventKind,
    MaterializedAttachment,
    Platform,
)


async def start_app(app: web.Application) -> tuple[web.AppRunner, str]:
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0)
    await site.start()
    port = site._server.sockets[0].getsockname()[1]
    return runner, f"http://127.0.0.1:{port}"


@pytest.mark.asyncio
async def test_ignores_system_and_other_bot_posts() -> None:
    async with aiohttp.ClientSession() as session:
        adapter = MattermostAdapter(
            session,
            base_url="http://mattermost.invalid",
            token="secret",
            channel_id="channel1",
        )
        adapter._users["bot2"] = {"id": "bot2", "username": "other", "is_bot": True}
        base = {
            "id": "post1",
            "channel_id": "channel1",
            "user_id": "bot2",
            "message": "automated",
            "create_at": 100,
            "update_at": 100,
            "root_id": "",
        }
        bot_event = await adapter.event_from_post(
            base, kind=EventKind.MESSAGE, self_user_id="bridge-bot"
        )
        system_event = await adapter.event_from_post(
            {**base, "id": "post2", "user_id": "someone", "type": "system_join_channel"},
            kind=EventKind.MESSAGE,
            self_user_id="bridge-bot",
        )
    assert bot_event.kind is EventKind.IGNORED
    assert system_event.kind is EventKind.IGNORED


@pytest.mark.asyncio
async def test_routes_posts_from_multiple_mattermost_channels() -> None:
    async with aiohttp.ClientSession() as session:
        adapter = MattermostAdapter(
            session,
            base_url="http://mattermost.invalid",
            token="secret",
            routes={"a": "channel-a", "b": "channel-b"},
        )
        adapter._users["employee"] = {
            "id": "employee",
            "username": "employee",
            "first_name": "Employee",
        }
        event = await adapter.event_from_post(
            {
                "id": "post-b",
                "channel_id": "channel-b",
                "user_id": "employee",
                "message": "from b",
                "create_at": 100,
                "update_at": 100,
                "root_id": "",
            },
            kind=EventKind.MESSAGE,
            self_user_id="bridge-bot",
        )

    assert (event.route_id, event.text) == ("b", "from b")
    assert event.checkpoint_key == "mattermost_checkpoint_ms:b"


@pytest.mark.asyncio
async def test_does_not_react_to_delivered_mattermost_post() -> None:
    created: list[dict[str, str]] = []

    async def me(_: web.Request) -> web.Response:
        return web.json_response({"id": "bridge-bot"})

    async def reactions(_: web.Request) -> web.Response:
        return web.json_response(None)

    async def create_reaction(request: web.Request) -> web.Response:
        created.append(await request.json())
        return web.json_response(created[-1])

    app = web.Application()
    app.router.add_get("/api/v4/users/me", me)
    app.router.add_get("/api/v4/posts/post1/reactions", reactions)
    app.router.add_post("/api/v4/reactions", create_reaction)
    runner, base = await start_app(app)
    event = BridgeEvent(
        event_id="mm:post1",
        platform=Platform.MATTERMOST,
        kind=EventKind.MESSAGE,
        message_id="post1",
        message_ids=("post1",),
        author_id="employee",
        author_name="Employee",
    )
    try:
        async with aiohttp.ClientSession() as session:
            adapter = MattermostAdapter(
                session,
                base_url=base,
                token="secret",
                channel_id="channel1",
            )
            await adapter.acknowledge_delivery(event)
    finally:
        await runner.cleanup()
    assert created == []


@pytest.mark.asyncio
async def test_sends_telegram_identity_and_avatar_in_mattermost_post() -> None:
    posts: list[dict[str, object]] = []

    async def client_config(_: web.Request) -> web.Response:
        return web.json_response(
            {"EnablePostUsernameOverride": "true", "EnablePostIconOverride": "true"}
        )

    async def create_post(request: web.Request) -> web.Response:
        posts.append(await request.json())
        return web.json_response({"id": "mm-post"})

    app = web.Application()
    app.router.add_get("/api/v4/config/client", client_config)
    app.router.add_post("/api/v4/posts", create_post)
    runner, base = await start_app(app)
    event = BridgeEvent(
        event_id="tg:10",
        platform=Platform.TELEGRAM,
        kind=EventKind.MESSAGE,
        message_id="10",
        message_ids=("10",),
        author_id="20",
        author_name="Contractor",
        text="hello",
    )
    sent = []

    async def on_sent(message) -> None:
        sent.append(message)

    try:
        async with aiohttp.ClientSession() as session:
            adapter = MattermostAdapter(
                session,
                base_url=base,
                token="secret",
                channel_id="channel1",
            )
            await adapter.send(
                event,
                DeliveryContext(reply_to_id=None, root_id=None, anchor_id=None),
                [],
                [],
                0,
                on_sent,
                author_avatar=MaterializedAttachment(
                    file_name="avatar.jpg",
                    mime_type="image/jpeg",
                    data=b"JPEG",
                ),
            )
    finally:
        await runner.cleanup()
    assert posts[0]["props"] == {
        "from_telegram_mattermost_bridge": True,
        "override_username": "🟠 Contractor · Telegram",
        "override_icon_url": "data:image/jpeg;base64,SlBFRw==",
    }
    assert posts[0]["message"] == "Contractor: hello"


@pytest.mark.asyncio
async def test_edits_linked_mattermost_post_without_correction_notice() -> None:
    patches: list[dict[str, object]] = []

    async def patch_post(request: web.Request) -> web.Response:
        patches.append(await request.json())
        return web.json_response({"id": "mm-post"})

    app = web.Application()
    app.router.add_put("/api/v4/posts/mm-post/patch", patch_post)
    runner, base = await start_app(app)
    event = BridgeEvent(
        event_id="tg:11:edit",
        platform=Platform.TELEGRAM,
        kind=EventKind.EDIT,
        message_id="10",
        message_ids=("10",),
        author_id="20",
        author_name="Антон",
        text="Спасибо",
    )
    try:
        async with aiohttp.ClientSession() as session:
            adapter = MattermostAdapter(
                session,
                base_url=base,
                token="secret",
                channel_id="channel1",
            )
            await adapter.edit(event, "mm-post")
    finally:
        await runner.cleanup()
    assert patches == [{"message": "Антон: Спасибо"}]


@pytest.mark.asyncio
async def test_sends_telegram_message_to_routed_mattermost_channel() -> None:
    channel_ids: list[str] = []

    async def client_config(_: web.Request) -> web.Response:
        return web.json_response({})

    async def create_post(request: web.Request) -> web.Response:
        body = await request.json()
        channel_ids.append(body["channel_id"])
        return web.json_response({"id": "mm-post"})

    app = web.Application()
    app.router.add_get("/api/v4/config/client", client_config)
    app.router.add_post("/api/v4/posts", create_post)
    runner, base = await start_app(app)
    event = BridgeEvent(
        event_id="tg:20:message",
        platform=Platform.TELEGRAM,
        kind=EventKind.MESSAGE,
        message_id="20",
        message_ids=("20",),
        author_id="contractor",
        author_name="Contractor",
        route_id="b",
        text="hello",
    )

    async def on_sent(_) -> None:
        return None

    try:
        async with aiohttp.ClientSession() as session:
            adapter = MattermostAdapter(
                session,
                base_url=base,
                token="secret",
                routes={"a": "channel-a", "b": "channel-b"},
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
    assert channel_ids == ["channel-b"]


@pytest.mark.asyncio
async def test_catches_up_all_routed_mattermost_channels() -> None:
    seen_since: dict[str, str] = {}

    async def posts(request: web.Request) -> web.Response:
        channel_id = request.match_info["channel_id"]
        seen_since[channel_id] = request.query["since"]
        post_id = "post-a" if channel_id == "channel-a" else "post-b"
        return web.json_response(
            {
                "order": [post_id],
                "posts": {
                    post_id: {
                        "id": post_id,
                        "channel_id": channel_id,
                        "create_at": 100 if channel_id == "channel-a" else 200,
                    }
                },
            }
        )

    app = web.Application()
    app.router.add_get("/api/v4/channels/{channel_id}/posts", posts)
    runner, base = await start_app(app)
    try:
        async with aiohttp.ClientSession() as session:
            adapter = MattermostAdapter(
                session,
                base_url=base,
                token="secret",
                routes={"a": "channel-a", "b": "channel-b"},
            )
            recovered = await adapter.catch_up({"a": 50, "b": 150})
    finally:
        await runner.cleanup()
    assert [post["id"] for post in recovered] == ["post-a", "post-b"]
    assert seen_since == {"channel-a": "50", "channel-b": "150"}


@pytest.mark.asyncio
async def test_reconciles_only_bridge_bot_basic_mattermost_reactions() -> None:
    created: list[dict[str, str]] = []
    deleted: list[str] = []

    async def reactions(_: web.Request) -> web.Response:
        return web.json_response(
            [
                {"user_id": "bridge-bot", "post_id": "post1", "emoji_name": "+1"},
                {"user_id": "bridge-bot", "post_id": "post1", "emoji_name": "fire"},
                {"user_id": "person", "post_id": "post1", "emoji_name": "joy"},
            ]
        )

    async def create_reaction(request: web.Request) -> web.Response:
        created.append(await request.json())
        return web.json_response(created[-1])

    async def delete_reaction(request: web.Request) -> web.Response:
        deleted.append(request.match_info["emoji"])
        return web.json_response({"status": "OK"})

    app = web.Application()
    app.router.add_get("/api/v4/posts/post1/reactions", reactions)
    app.router.add_post("/api/v4/reactions", create_reaction)
    app.router.add_delete(
        "/api/v4/users/{user}/posts/{post}/reactions/{emoji}", delete_reaction
    )
    runner, base = await start_app(app)
    event = BridgeEvent(
        event_id="tg:reaction",
        platform=Platform.TELEGRAM,
        kind=EventKind.REACTION_ADDED,
        message_id="10",
        message_ids=("10",),
        author_id="contractor",
        author_name="Contractor",
        reaction="❤️",
    )
    try:
        async with aiohttp.ClientSession() as session:
            adapter = MattermostAdapter(
                session,
                base_url=base,
                token="secret",
                channel_id="channel1",
            )
            adapter._self_user_id = "bridge-bot"
            await adapter.sync_reactions(event, "post1", ("👍", "❤️"))
    finally:
        await runner.cleanup()

    assert created == [
        {"user_id": "bridge-bot", "post_id": "post1", "emoji_name": "heart"}
    ]
    assert deleted == ["fire"]
