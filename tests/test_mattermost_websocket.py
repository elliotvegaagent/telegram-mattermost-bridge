from __future__ import annotations

import asyncio
import json

import aiohttp
import pytest
from aiohttp import web

from bridge.adapters.mattermost import MattermostAdapter
from bridge.models import EventKind


@pytest.mark.asyncio
async def test_consumes_posted_event_from_fake_websocket() -> None:
    async def websocket(request: web.Request) -> web.WebSocketResponse:
        socket = web.WebSocketResponse()
        await socket.prepare(request)
        auth = await socket.receive_json()
        assert auth["action"] == "authentication_challenge"
        post = {
            "id": "post1",
            "channel_id": "channel1",
            "user_id": "user1",
            "message": "hello",
            "create_at": 100,
            "update_at": 100,
            "root_id": "",
        }
        await socket.send_json({"event": "posted", "data": {"post": json.dumps(post)}})
        await asyncio.sleep(0.1)
        return socket

    async def user(_: web.Request) -> web.Response:
        return web.json_response({"id": "user1", "username": "employee", "first_name": "E"})

    app = web.Application()
    app.router.add_get("/api/v4/websocket", websocket)
    app.router.add_get("/api/v4/users/user1", user)
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0)
    await site.start()
    port = site._server.sockets[0].getsockname()[1]
    received = []
    stop = asyncio.Event()
    try:
        async with aiohttp.ClientSession() as session:
            adapter = MattermostAdapter(
                session,
                base_url=f"http://127.0.0.1:{port}",
                token="secret",
                channel_id="channel1",
            )

            async def accept(event):
                received.append(event)
                stop.set()
                return True

            await adapter._consume_socket(
                checkpoint_ref={"default": 0},
                self_user_id="bot1",
                accept=accept,
                stop=stop,
            )
    finally:
        await runner.cleanup()
    assert len(received) == 1
    assert received[0].kind is EventKind.MESSAGE
    assert received[0].author_name == "E"


@pytest.mark.asyncio
async def test_consumes_reaction_event_and_ignores_bridge_bot_reaction() -> None:
    async def websocket(request: web.Request) -> web.WebSocketResponse:
        socket = web.WebSocketResponse()
        await socket.prepare(request)
        await socket.receive_json()
        for user_id in ("user1", "bot1"):
            reaction = {
                "user_id": user_id,
                "post_id": "post1",
                "emoji_name": "+1",
                "create_at": 100,
            }
            await socket.send_json(
                {
                    "event": "reaction_added",
                    "data": {"reaction": json.dumps(reaction)},
                    "broadcast": {"channel_id": "channel1"},
                }
            )
        await asyncio.sleep(0.1)
        return socket

    app = web.Application()
    app.router.add_get("/api/v4/websocket", websocket)
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0)
    await site.start()
    port = site._server.sockets[0].getsockname()[1]
    received = []
    stop = asyncio.Event()
    try:
        async with aiohttp.ClientSession() as session:
            adapter = MattermostAdapter(
                session,
                base_url=f"http://127.0.0.1:{port}",
                token="secret",
                channel_id="channel1",
            )

            async def accept(event):
                received.append(event)
                if len(received) == 2:
                    stop.set()
                return True

            await adapter._consume_socket(
                checkpoint_ref={"default": 0},
                self_user_id="bot1",
                accept=accept,
                stop=stop,
            )
    finally:
        await runner.cleanup()

    assert received[0].kind is EventKind.REACTION_ADDED
    assert received[0].reaction == "👍"
    assert received[1].kind is EventKind.IGNORED
