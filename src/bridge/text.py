from __future__ import annotations

import hashlib
import re

from .models import BridgeEvent, EventKind, Platform

_MM_MARKDOWN = re.compile(r"([\\`*_{}\[\]()<>#+\-.!|~])")
_MM_MENTION = re.compile(r"@(?=[\w.-])", re.UNICODE)
_AUTHOR_MARKERS = ("🔵", "🟢", "🟣", "🟠", "🟡", "🟤", "⚫")


def author_marker(event: BridgeEvent) -> str:
    identity = f"{event.platform.value}:{event.author_id}".encode()
    return _AUTHOR_MARKERS[hashlib.sha256(identity).digest()[0] % len(_AUTHOR_MARKERS)]


def author_label(event: BridgeEvent) -> str:
    suffix = "Mattermost" if event.platform is Platform.MATTERMOST else "Telegram"
    username = f" (@{event.author_username})" if event.author_username else ""
    return f"{author_marker(event)} {event.author_name} · {suffix}{username}"


def render_text(
    event: BridgeEvent,
    target: Platform,
    unknown_quote: str | None = None,
    *,
    include_author: bool = True,
) -> str:
    if event.kind is EventKind.EDIT:
        marker = "✏️ Исправление"
    elif event.kind is EventKind.DELETE:
        marker = "🗑️ Сообщение удалено"
    else:
        marker = ""

    body = event.text.strip()
    if unknown_quote:
        quote = unknown_quote.replace("\n", " ").strip()[:240]
        body = f"↩ Ответ на несвязанное сообщение: «{quote}»\n\n{body}".strip()
    if marker:
        body = f"{marker}: {body}".rstrip(": ")
    if include_author and event.platform is Platform.MATTERMOST and target is Platform.TELEGRAM:
        result = f"{event.author_name} : {body}".rstrip(" :")
    elif include_author and event.platform is Platform.TELEGRAM and target is Platform.MATTERMOST:
        result = f"{event.author_name}: {body}".rstrip(": ")
    elif include_author:
        result = f"{author_label(event)}\n{body}".rstrip()
    else:
        result = body

    if target is Platform.MATTERMOST:
        # External text must not create Mattermost formatting or notifications.
        result = _MM_MARKDOWN.sub(r"\\\1", result)
        result = _MM_MENTION.sub("@\u200b", result)
    return result


def split_text(text: str, limit: int) -> list[str]:
    if len(text) <= limit:
        return [text]
    # Reserve room for a stable (N/M) prefix. Recompute if total grows to 3 digits.
    chunk_size = max(1, limit - 12)
    chunks: list[str] = []
    remaining = text
    while remaining:
        if len(remaining) <= chunk_size:
            chunks.append(remaining)
            break
        cut = remaining.rfind("\n", 0, chunk_size + 1)
        if cut < chunk_size // 2:
            cut = remaining.rfind(" ", 0, chunk_size + 1)
        if cut < chunk_size // 2:
            cut = chunk_size
        chunks.append(remaining[:cut].rstrip())
        remaining = remaining[cut:].lstrip()
    total = len(chunks)
    return [f"({index}/{total}) {chunk}" for index, chunk in enumerate(chunks, 1)]


def safe_filename(name: str) -> str:
    cleaned = name.replace("/", "_").replace("\\", "_").replace("\x00", "")
    cleaned = re.sub(r"[\r\n\t]+", " ", cleaned).strip()
    return cleaned[:240] or "attachment"
