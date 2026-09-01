from __future__ import annotations

import json
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any


class ConfigError(ValueError):
    """Raised when required configuration is missing or invalid."""


def _required(name: str) -> str:
    value = os.environ.get(name, "").strip()
    if not value:
        raise ConfigError(f"{name} is required")
    return value


@dataclass(frozen=True, slots=True)
class BridgeRoute:
    id: str
    tg_chat_id: int
    mm_channel_id: str


def _bridge_routes() -> tuple[BridgeRoute, ...]:
    raw = os.environ.get("BRIDGE_PAIRS", "").strip()
    if raw:
        try:
            payload: Any = json.loads(raw)
        except json.JSONDecodeError as error:
            raise ConfigError("BRIDGE_PAIRS must be valid JSON") from error
        if not isinstance(payload, list) or not payload:
            raise ConfigError("BRIDGE_PAIRS must be a non-empty JSON array")
        routes: list[BridgeRoute] = []
        for index, value in enumerate(payload):
            if not isinstance(value, dict):
                raise ConfigError(f"BRIDGE_PAIRS[{index}] must be an object")
            try:
                route = BridgeRoute(
                    id=str(value["id"]).strip(),
                    tg_chat_id=int(value["tg_chat_id"]),
                    mm_channel_id=str(value["mm_channel_id"]).strip(),
                )
            except (KeyError, TypeError, ValueError) as error:
                raise ConfigError(
                    f"BRIDGE_PAIRS[{index}] requires id, integer tg_chat_id and mm_channel_id"
                ) from error
            if not route.id or not route.mm_channel_id:
                raise ConfigError(f"BRIDGE_PAIRS[{index}] contains an empty id or channel")
            routes.append(route)
    else:
        try:
            tg_chat_id = int(_required("TG_CHAT_ID"))
        except ValueError as error:
            raise ConfigError("TG_CHAT_ID must be an integer") from error
        routes = [
            BridgeRoute(
                id="default",
                tg_chat_id=tg_chat_id,
                mm_channel_id=_required("MM_CHANNEL_ID"),
            )
        ]

    if len({route.id for route in routes}) != len(routes):
        raise ConfigError("BRIDGE_PAIRS contains duplicate ids")
    if len({route.tg_chat_id for route in routes}) != len(routes):
        raise ConfigError("BRIDGE_PAIRS contains duplicate Telegram chat IDs")
    if len({route.mm_channel_id for route in routes}) != len(routes):
        raise ConfigError("BRIDGE_PAIRS contains duplicate Mattermost channel IDs")
    return tuple(routes)


@dataclass(frozen=True, slots=True)
class Settings:
    mm_url: str
    mm_bot_token: str
    tg_bot_token: str
    routes: tuple[BridgeRoute, ...]
    db_path: Path
    http_host: str = "0.0.0.0"
    http_port: int = 8080
    log_level: str = "INFO"
    max_attachment_bytes: int = 25 * 1024 * 1024
    tg_api_base: str = "https://api.telegram.org"

    @classmethod
    def from_env(cls) -> Settings:
        bind = os.environ.get("HTTP_BIND", "0.0.0.0:8080").strip()
        try:
            host, port_text = bind.rsplit(":", 1)
            port = int(port_text)
        except (ValueError, TypeError) as error:
            raise ConfigError("HTTP_BIND must have the form host:port") from error
        if not host or not 1 <= port <= 65535:
            raise ConfigError("HTTP_BIND contains an invalid host or port")

        try:
            max_bytes = int(os.environ.get("MAX_ATTACHMENT_BYTES", str(25 * 1024 * 1024)))
        except ValueError as error:
            raise ConfigError("MAX_ATTACHMENT_BYTES must be an integer") from error
        if max_bytes <= 0:
            raise ConfigError("MAX_ATTACHMENT_BYTES must be positive")

        return cls(
            mm_url=_required("MM_URL").rstrip("/"),
            mm_bot_token=_required("MM_BOT_TOKEN"),
            tg_bot_token=_required("TG_BOT_TOKEN"),
            routes=_bridge_routes(),
            db_path=Path(os.environ.get("DB_PATH", "/data/bridge.db")).expanduser(),
            http_host=host,
            http_port=port,
            log_level=os.environ.get("LOG_LEVEL", "INFO").upper(),
            max_attachment_bytes=max_bytes,
            tg_api_base=os.environ.get("TG_API_BASE", "https://api.telegram.org").rstrip("/"),
        )
