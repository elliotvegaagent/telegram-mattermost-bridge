from __future__ import annotations

from collections.abc import Awaitable, Callable
from typing import Protocol

from .models import (
    Attachment,
    BridgeEvent,
    DeliveryContext,
    MaterializedAttachment,
    Platform,
    SentMessage,
)


class AttachmentRejected(ValueError):
    pass


SentCallback = Callable[[SentMessage], Awaitable[None]]


class PlatformAdapter(Protocol):
    platform: Platform

    async def fetch_attachment(
        self, attachment: Attachment, max_bytes: int
    ) -> MaterializedAttachment: ...

    async def fetch_author_avatar(
        self, event: BridgeEvent, max_bytes: int
    ) -> MaterializedAttachment | None: ...

    async def send(
        self,
        event: BridgeEvent,
        context: DeliveryContext,
        attachments: list[MaterializedAttachment],
        attachment_warnings: list[str],
        completed_parts: int,
        on_sent: SentCallback,
        *,
        author_avatar: MaterializedAttachment | None = None,
    ) -> None: ...

    async def send_failure_notice(self, event: BridgeEvent, reason: str) -> None: ...

    async def edit(self, event: BridgeEvent, target_message_id: str) -> None: ...

    async def acknowledge_delivery(self, event: BridgeEvent) -> None: ...

    async def sync_reactions(
        self,
        event: BridgeEvent,
        target_message_id: str,
        reactions: tuple[str, ...],
    ) -> None: ...
