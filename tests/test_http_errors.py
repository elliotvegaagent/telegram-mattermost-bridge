from __future__ import annotations

import aiohttp
import pytest
from aiohttp import web

from bridge.adapters.mattermost import MattermostAdapter
from bridge.adapters.telegram import TelegramAdapter
from bridge.models import DeliveryError


async def start_app(app: web.Application) -> tuple[web.AppRunner, str]:
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0)
    await site.start()
    port = site._server.sockets[0].getsockname()[1]
    return runner, f"http://127.0.0.1:{port}"


@pytest.mark.asyncio
async def test_telegram_honors_retry_after() -> None:
    async def limited(_: web.Request) -> web.Response:
        return web.json_response(
            {"ok": False, "description": "Too Many Requests", "parameters": {"retry_after": 17}},
            status=429,
        )

    app = web.Application()
    app.router.add_post("/botsecret/getMe", limited)
    runner, base = await start_app(app)
    try:
        async with aiohttp.ClientSession() as session:
            adapter = TelegramAdapter(session, token="secret", chat_id=-100, api_base=base)
            with pytest.raises(DeliveryError) as captured:
                await adapter.get_me()
        assert captured.value.retryable
        assert captured.value.retry_after == 17
    finally:
        await runner.cleanup()


@pytest.mark.asyncio
async def test_mattermost_403_is_permanent() -> None:
    async def forbidden(_: web.Request) -> web.Response:
        return web.json_response({"message": "forbidden"}, status=403)

    app = web.Application()
    app.router.add_get("/api/v4/users/me", forbidden)
    runner, base = await start_app(app)
    try:
        async with aiohttp.ClientSession() as session:
            adapter = MattermostAdapter(
                session, base_url=base, token="secret", channel_id="channel"
            )
            with pytest.raises(DeliveryError) as captured:
                await adapter.get_me()
        assert not captured.value.retryable
    finally:
        await runner.cleanup()
