from __future__ import annotations

import asyncio
import json
import logging
import signal
import time
from dataclasses import dataclass

import aiohttp

from .adapters.mattermost import MattermostAdapter
from .adapters.telegram import TelegramAdapter
from .config import Settings
from .core import Bridge
from .health import HealthState, create_health_app, run_health_server
from .models import Platform
from .storage import Storage


@dataclass(slots=True)
class RuntimeParts:
    session: aiohttp.ClientSession
    telegram: TelegramAdapter
    mattermost: MattermostAdapter


async def _parts(settings: Settings, health: HealthState) -> RuntimeParts:
    timeout = aiohttp.ClientTimeout(total=None, connect=20, sock_connect=20)
    session = aiohttp.ClientSession(timeout=timeout, raise_for_status=False)
    return RuntimeParts(
        session=session,
        telegram=TelegramAdapter(
            session,
            token=settings.tg_bot_token,
            routes={route.id: route.tg_chat_id for route in settings.routes},
            api_base=settings.tg_api_base,
            health_callback=health.callback("telegram"),
        ),
        mattermost=MattermostAdapter(
            session,
            base_url=settings.mm_url,
            token=settings.mm_bot_token,
            routes={route.id: route.mm_channel_id for route in settings.routes},
            health_callback=health.callback("mattermost"),
        ),
    )


async def doctor(settings: Settings, storage: Storage) -> dict[str, object]:
    health = HealthState()
    parts = await _parts(settings, health)
    try:
        tg_me, mm_me = await asyncio.gather(
            parts.telegram.get_me(),
            parts.mattermost.get_me(),
        )
        route_checks = await asyncio.gather(
            *(
                asyncio.gather(
                    parts.telegram.get_chat(route.id),
                    parts.telegram.get_chat_member(str(tg_me["id"]), route.id),
                    parts.mattermost.get_channel(route.id),
                    parts.mattermost.get_channel_member(str(mm_me["id"]), route.id),
                )
                for route in settings.routes
            )
        )
        storage.set_state("doctor_last_run_ms", str(int(time.time() * 1000)))
        return {
            "status": "ok",
            "telegram_bot": tg_me.get("username") or tg_me.get("id"),
            "mattermost_bot": mm_me.get("username") or mm_me.get("id"),
            "pairs": [
                {
                    "id": route.id,
                    "telegram": {
                        "chat_id": tg_chat.get("id"),
                        "chat_title": tg_chat.get("title") or tg_chat.get("username"),
                        "membership": tg_member.get("status"),
                    },
                    "mattermost": {
                        "channel_id": mm_channel.get("id"),
                        "channel": mm_channel.get("display_name") or mm_channel.get("name"),
                    },
                }
                for route, (tg_chat, tg_member, mm_channel, _) in zip(
                    settings.routes, route_checks, strict=True
                )
            ],
            "database": str(settings.db_path),
            "initialized": storage.is_initialized(),
        }
    finally:
        await parts.session.close()


async def initialize(settings: Settings, storage: Storage, *, force: bool = False) -> dict[str, object]:
    if storage.is_initialized() and not force:
        raise RuntimeError("bridge is already initialized; use --force to reset checkpoints")
    result = await doctor(settings, storage)
    health = HealthState()
    parts = await _parts(settings, health)
    try:
        await parts.telegram.drop_pending_updates()
    finally:
        await parts.session.close()
    storage.initialize(
        mm_checkpoint_ms=int(time.time() * 1000),
        tg_offset=0,
        route_ids=tuple(route.id for route in settings.routes),
    )
    result["initialized"] = True
    result["history_policy"] = "pending Telegram updates dropped; Mattermost starts now"
    return result


async def run(settings: Settings, storage: Storage) -> None:
    if not storage.is_initialized():
        raise RuntimeError("bridge is not initialized; run `python -m bridge init` first")
    health = HealthState()
    parts = await _parts(settings, health)
    stop = asyncio.Event()
    loop = asyncio.get_running_loop()
    for name in ("SIGINT", "SIGTERM"):
        sig = getattr(signal, name, None)
        if sig is not None:
            try:
                loop.add_signal_handler(sig, stop.set)
            except NotImplementedError:
                pass

    try:
        tg_me, mm_me = await asyncio.gather(parts.telegram.get_me(), parts.mattermost.get_me())
        bridge = Bridge(
            storage,
            {
                Platform.TELEGRAM: parts.telegram,
                Platform.MATTERMOST: parts.mattermost,
            },
            routes={route.id: route for route in settings.routes},
            max_attachment_bytes=settings.max_attachment_bytes,
        )
        telegram_offset = int(storage.get_state("telegram_offset", "0") or "0")
        startup_ms = int(time.time() * 1000)
        legacy_mm_checkpoint = int(
            storage.get_state("mattermost_checkpoint_ms", str(startup_ms)) or startup_ms
        )
        mm_checkpoints: dict[str, int] = {}
        for route in settings.routes:
            key = f"mattermost_checkpoint_ms:{route.id}"
            value = storage.get_state(key)
            if value is None:
                checkpoint = legacy_mm_checkpoint if route.id == "default" else startup_ms
                storage.set_state(key, str(checkpoint))
            else:
                checkpoint = int(value)
            mm_checkpoints[route.id] = checkpoint
        health_app = create_health_app(health, storage)
        logging.getLogger("bridge.runtime").info("bridge_started")
        async with asyncio.TaskGroup() as group:
            group.create_task(bridge.run_outbox(stop), name="outbox")
            group.create_task(
                parts.telegram.run_polling(
                    initial_offset=telegram_offset,
                    self_user_id=str(tg_me["id"]),
                    accept=bridge.accept,
                    stop=stop,
                ),
                name="telegram-polling",
            )
            group.create_task(
                parts.mattermost.run_websocket(
                    initial_checkpoints=mm_checkpoints,
                    self_user_id=str(mm_me["id"]),
                    accept=bridge.accept,
                    stop=stop,
                ),
                name="mattermost-websocket",
            )
            group.create_task(
                run_health_server(
                    health_app,
                    host=settings.http_host,
                    port=settings.http_port,
                    stop=stop,
                ),
                name="health-server",
            )
    finally:
        await parts.session.close()


def print_result(result: dict[str, object]) -> None:
    print(json.dumps(result, ensure_ascii=False, indent=2))
