from __future__ import annotations

import json
from dataclasses import asdict, dataclass, field
from enum import StrEnum
from typing import Any


class Platform(StrEnum):
    TELEGRAM = "telegram"
    MATTERMOST = "mattermost"

    @property
    def opposite(self) -> Platform:
        return Platform.MATTERMOST if self is Platform.TELEGRAM else Platform.TELEGRAM


class EventKind(StrEnum):
    MESSAGE = "message"
    EDIT = "edit"
    DELETE = "delete"
    REACTION_ADDED = "reaction_added"
    REACTION_REMOVED = "reaction_removed"
    IGNORED = "ignored"


@dataclass(frozen=True, slots=True)
class Attachment:
    file_id: str
    file_name: str
    mime_type: str = "application/octet-stream"
    size: int | None = None
    source_message_id: str | None = None


@dataclass(frozen=True, slots=True)
class MaterializedAttachment:
    file_name: str
    mime_type: str
    data: bytes
    source_message_id: str | None = None


@dataclass(frozen=True, slots=True)
class BridgeEvent:
    event_id: str
    platform: Platform
    kind: EventKind
    message_id: str
    message_ids: tuple[str, ...]
    author_id: str
    author_name: str
    route_id: str = "default"
    author_username: str | None = None
    text: str = ""
    reply_to_id: str | None = None
    reply_quote: str | None = None
    root_id: str | None = None
    created_at_ms: int = 0
    attachments: tuple[Attachment, ...] = field(default_factory=tuple)
    reaction: str | None = None
    checkpoint_key: str | None = None
    checkpoint_value: str | None = None

    def to_json(self) -> str:
        value = asdict(self)
        value["platform"] = self.platform.value
        value["kind"] = self.kind.value
        value["message_ids"] = list(self.message_ids)
        value["attachments"] = [asdict(item) for item in self.attachments]
        return json.dumps(value, ensure_ascii=False, separators=(",", ":"))

    @classmethod
    def from_json(cls, payload: str) -> BridgeEvent:
        value: dict[str, Any] = json.loads(payload)
        value["platform"] = Platform(value["platform"])
        value["kind"] = EventKind(value["kind"])
        value["message_ids"] = tuple(value.get("message_ids") or [value["message_id"]])
        value["attachments"] = tuple(Attachment(**item) for item in value.get("attachments", []))
        return cls(**value)


@dataclass(frozen=True, slots=True)
class MessageLink:
    mm_post_id: str
    tg_chat_id: int
    tg_message_id: str
    mm_root_id: str
    tg_anchor_message_id: str
    part_index: int = 0


@dataclass(frozen=True, slots=True)
class DeliveryContext:
    reply_to_id: str | None
    root_id: str | None
    anchor_id: str | None
    unknown_reply_quote: str | None = None
    edit_target_id: str | None = None


@dataclass(frozen=True, slots=True)
class SentMessage:
    message_id: str
    root_id: str | None = None
    part_index: int = 0


@dataclass(frozen=True, slots=True)
class OutboxJob:
    id: int
    event_id: str
    target: Platform
    attempts: int
    progress: dict[str, Any]


class DeliveryError(RuntimeError):
    def __init__(
        self,
        message: str,
        *,
        retryable: bool,
        retry_after: float | None = None,
    ) -> None:
        super().__init__(message)
        self.retryable = retryable
        self.retry_after = retry_after
