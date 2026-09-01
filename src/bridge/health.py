from __future__ import annotations

import time
from dataclasses import dataclass, field

from aiohttp import web

from .storage import Storage


@dataclass(slots=True)
class AdapterStatus:
    healthy: bool = False
    last_success_at: float | None = None


@dataclass(slots=True)
class HealthState:
    telegram: AdapterStatus = field(default_factory=AdapterStatus)
    mattermost: AdapterStatus = field(default_factory=AdapterStatus)

    def callback(self, name: str):
        status = getattr(self, name)

        def update(healthy: bool) -> None:
            status.healthy = healthy
            if healthy:
                status.last_success_at = time.time()

        return update


def create_health_app(state: HealthState, storage: Storage) -> web.Application:
    app = web.Application()

    async def healthz(_: web.Request) -> web.Response:
        return web.json_response({"status": "ok"})

    async def readyz(_: web.Request) -> web.Response:
        pending_age = storage.oldest_pending_age_seconds()
        counts = storage.counts()
        ready = (
            state.telegram.healthy
            and state.mattermost.healthy
            and (pending_age is None or pending_age <= 600)
        )
        body = {
            "status": "ready" if ready else "degraded",
            "telegram": state.telegram.healthy,
            "mattermost": state.mattermost.healthy,
            "oldest_pending_seconds": pending_age,
            "outbox": counts,
        }
        return web.json_response(body, status=200 if ready else 503)

    app.router.add_get("/healthz", healthz)
    app.router.add_get("/readyz", readyz)
    return app


async def run_health_server(
    app: web.Application, *, host: str, port: int, stop
) -> None:
    runner = web.AppRunner(app, access_log=None)
    await runner.setup()
    site = web.TCPSite(runner, host=host, port=port)
    await site.start()
    try:
        await stop.wait()
    finally:
        await runner.cleanup()
