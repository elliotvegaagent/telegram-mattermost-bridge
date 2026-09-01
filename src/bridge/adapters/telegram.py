from __future__ import annotations

import asyncio
import json
import logging
import mimetypes
import time
from collections.abc import Awaitable, Callable, Mapping
from dataclasses import replace
from typing import Any

import aiohttp

from ..models import (
    Attachment,
    BridgeEvent,
    DeliveryContext,
    DeliveryError,
    EventKind,
    MaterializedAttachment,
    Platform,
    SentMessage,
)
from ..ports import AttachmentRejected, SentCallback
from ..reactions import telegram_reaction
from ..text import render_text, safe_filename, split_text

HealthCallback = Callable[[bool], None]


class TelegramAdapter:
    platform = Platform.TELEGRAM

    def __init__(
        self,
        session: aiohttp.ClientSession,
        *,
        token: str,
        chat_id: int | None = None,
        routes: Mapping[str, int] | None = None,
        api_base: str = "https://api.telegram.org",
        health_callback: HealthCallback | None = None,
    ) -> None:
        self._session = session
        self._token = token
        if routes is None:
            if chat_id is None:
                raise ValueError("chat_id or routes is required")
            routes = {"default": chat_id}
        elif chat_id is not None:
            raise ValueError("use either chat_id or routes")
        self._chat_ids = dict(routes)
        if not self._chat_ids:
            raise ValueError("at least one Telegram route is required")
        self._routes_by_chat = {
            route_chat_id: route_id for route_id, route_chat_id in self._chat_ids.items()
        }
        self._chat_id = next(iter(self._chat_ids.values()))
        self._method_base = f"{api_base}/bot{token}"
        self._file_base = f"{api_base}/file/bot{token}"
        self._health_callback = health_callback or (lambda healthy: None)
        self._log = logging.getLogger("bridge.telegram")

    async def request(
        self,
        method: str,
        *,
        data: dict[str, Any] | aiohttp.FormData | None = None,
        timeout: float = 40,
    ) -> Any:
        try:
            async with self._session.post(
                f"{self._method_base}/{method}",
                data=data,
                timeout=aiohttp.ClientTimeout(total=timeout),
            ) as response:
                payload = await response.json(content_type=None)
        except (TimeoutError, aiohttp.ClientError) as error:
            self._health_callback(False)
            raise DeliveryError(type(error).__name__, retryable=True) from error
        if response.status == 429:
            retry_after = float(payload.get("parameters", {}).get("retry_after", 5))
            raise DeliveryError("Telegram rate limit", retryable=True, retry_after=retry_after)
        if response.status >= 500:
            raise DeliveryError(f"Telegram HTTP {response.status}", retryable=True)
        if response.status >= 400 or not payload.get("ok"):
            description = str(payload.get("description", f"HTTP {response.status}"))
            raise DeliveryError(f"Telegram rejected request: {description}", retryable=False)
        self._health_callback(True)
        return payload.get("result")

    async def get_me(self) -> dict[str, Any]:
        return await self.request("getMe")

    def _chat_id_for(self, route_id: str) -> int:
        try:
            return self._chat_ids[route_id]
        except KeyError as error:
            raise DeliveryError(
                f"unknown bridge route: {route_id}", retryable=False
            ) from error

    async def get_chat(self, route_id: str | None = None) -> dict[str, Any]:
        chat_id = self._chat_id if route_id is None else self._chat_id_for(route_id)
        return await self.request("getChat", data={"chat_id": str(chat_id)})

    async def get_chat_member(
        self, user_id: str, route_id: str | None = None
    ) -> dict[str, Any]:
        chat_id = self._chat_id if route_id is None else self._chat_id_for(route_id)
        return await self.request(
            "getChatMember",
            data={"chat_id": str(chat_id), "user_id": user_id},
        )

    async def drop_pending_updates(self) -> None:
        await self.request(
            "deleteWebhook", data={"drop_pending_updates": "true"}, timeout=15
        )

    async def get_updates(self, offset: int) -> list[dict[str, Any]]:
        result = await self.request(
            "getUpdates",
            data={
                "offset": str(offset),
                "timeout": "30",
                "allowed_updates": json.dumps(
                    ["message", "edited_message", "message_reaction"]
                ),
            },
            timeout=40,
        )
        return list(result or [])

    async def run_polling(
        self,
        *,
        initial_offset: int,
        self_user_id: str,
        accept: Callable[[BridgeEvent], Awaitable[bool]],
        stop: asyncio.Event,
    ) -> None:
        offset = initial_offset
        delay = 1.0
        while not stop.is_set():
            try:
                updates = await self.get_updates(offset)
                for event in self.events_from_updates(updates, self_user_id=self_user_id):
                    await accept(event)
                    if event.checkpoint_value is not None:
                        offset = max(offset, int(event.checkpoint_value))
                delay = 1.0
            except DeliveryError as error:
                self._log.warning("polling_retry")
                try:
                    await asyncio.wait_for(stop.wait(), timeout=error.retry_after or delay)
                except TimeoutError:
                    pass
                delay = min(60.0, delay * 2)

    def events_from_updates(
        self, updates: list[dict[str, Any]], *, self_user_id: str
    ) -> list[BridgeEvent]:
        grouped: list[list[tuple[int, dict[str, Any], EventKind]]] = []
        albums: dict[str, list[tuple[int, dict[str, Any], EventKind]]] = {}
        reaction_updates: list[tuple[int, dict[str, Any]]] = []
        for update in updates:
            update_id = int(update["update_id"])
            if "message_reaction" in update:
                reaction_updates.append((update_id, update["message_reaction"]))
                continue
            if "edited_message" in update:
                message = update["edited_message"]
                kind = EventKind.EDIT
            elif "message" in update:
                message = update["message"]
                kind = EventKind.MESSAGE
            else:
                grouped.append([(update_id, {}, EventKind.IGNORED)])
                continue
            media_group = message.get("media_group_id") if kind is EventKind.MESSAGE else None
            if media_group:
                albums.setdefault(str(media_group), []).append((update_id, message, kind))
            else:
                grouped.append([(update_id, message, kind)])
        grouped.extend(albums.values())
        normalized: list[tuple[int, list[BridgeEvent]]] = [
            (min(item[0] for item in items), [self._event_from_group(items, self_user_id)])
            for items in grouped
        ]
        normalized.extend(
            (update_id, self._events_from_reaction_update(update_id, value, self_user_id))
            for update_id, value in reaction_updates
        )
        normalized.sort(key=lambda item: item[0])
        return [event for _, events in normalized for event in events]

    def _events_from_reaction_update(
        self,
        update_id: int,
        value: dict[str, Any],
        self_user_id: str,
    ) -> list[BridgeEvent]:
        checkpoint = str(update_id + 1)
        chat_id = value.get("chat", {}).get("id")
        route_id = self._routes_by_chat.get(chat_id)
        sender = value.get("user") or value.get("actor_chat") or {}
        own_reaction = str(sender.get("id", "")) == self_user_id
        message_id = str(value.get("message_id", update_id))
        old = [
            reaction
            for item in value.get("old_reaction") or []
            if (reaction := telegram_reaction(item)) is not None
        ]
        new = [
            reaction
            for item in value.get("new_reaction") or []
            if (reaction := telegram_reaction(item)) is not None
        ]
        changes = [
            (EventKind.REACTION_REMOVED, reaction)
            for reaction in old
            if reaction not in new
        ] + [
            (EventKind.REACTION_ADDED, reaction)
            for reaction in new
            if reaction not in old
        ]
        if route_id is None or own_reaction or not changes:
            return [
                BridgeEvent(
                    event_id=f"tg:{update_id}:reaction:ignored",
                    platform=Platform.TELEGRAM,
                    kind=EventKind.IGNORED,
                    message_id=message_id,
                    message_ids=(message_id,),
                    author_id=str(sender.get("id", "")),
                    author_name="ignored",
                    route_id=route_id or "ignored",
                    checkpoint_key="telegram_offset",
                    checkpoint_value=checkpoint,
                )
            ]

        author_name = " ".join(
            part for part in (sender.get("first_name"), sender.get("last_name")) if part
        ) or sender.get("title") or sender.get("username") or str(sender.get("id", "unknown"))
        events = [
            BridgeEvent(
                event_id=f"tg:{update_id}:{kind.value}:{ord(reaction[0]):x}",
                platform=Platform.TELEGRAM,
                kind=kind,
                message_id=message_id,
                message_ids=(message_id,),
                author_id=str(sender.get("id", "")),
                author_name=author_name,
                route_id=route_id,
                author_username=sender.get("username"),
                reaction=reaction,
                created_at_ms=int(value.get("date", time.time())) * 1000,
                checkpoint_key="telegram_offset",
                checkpoint_value=None,
            )
            for kind, reaction in changes
        ]
        events[-1] = replace(events[-1], checkpoint_value=checkpoint)
        return events

    def _event_from_group(
        self,
        items: list[tuple[int, dict[str, Any], EventKind]],
        self_user_id: str,
    ) -> BridgeEvent:
        update_ids = [item[0] for item in items]
        first = items[0][1]
        checkpoint = str(max(update_ids) + 1)
        chat_id = first.get("chat", {}).get("id")
        route_id = self._routes_by_chat.get(chat_id)
        sender = first.get("from") or first.get("sender_chat") or {}
        own_message = str(sender.get("id", "")) == self_user_id
        has_supported_content = any(
            key in first
            for key in (
                "text",
                "caption",
                "photo",
                "document",
                "video",
                "audio",
                "voice",
                "animation",
                "sticker",
                "poll",
            )
        )
        if not first or route_id is None or own_message or not has_supported_content:
            return BridgeEvent(
                event_id=f"tg:ignored:{min(update_ids)}:{max(update_ids)}",
                platform=Platform.TELEGRAM,
                kind=EventKind.IGNORED,
                message_id=str(first.get("message_id", min(update_ids))),
                message_ids=(str(first.get("message_id", min(update_ids))),),
                author_id=str(sender.get("id", "")),
                author_name="ignored",
                route_id=route_id or "ignored",
                checkpoint_key="telegram_offset",
                checkpoint_value=checkpoint,
            )

        kind = items[0][2]
        messages = [item[1] for item in items]
        message_ids = tuple(str(message["message_id"]) for message in messages)
        attachments: list[Attachment] = []
        text = ""
        for message in messages:
            text = text or message.get("text") or message.get("caption") or ""
            attachment = self._attachment_from_message(message)
            if attachment:
                attachments.append(attachment)
        if not text and first.get("sticker"):
            text = "[Стикер не поддерживается]"
        if not text and first.get("poll"):
            text = f"[Опрос: {first['poll'].get('question', '')}]"

        reply = first.get("reply_to_message") or {}
        reply_text = reply.get("text") or reply.get("caption") or ""
        author_name = " ".join(
            part for part in (sender.get("first_name"), sender.get("last_name")) if part
        ) or sender.get("title") or sender.get("username") or str(sender.get("id", "unknown"))
        album_id = first.get("media_group_id")
        if album_id:
            # The range keeps retries idempotent without losing a rare album split
            # across two getUpdates responses.
            event_id = (
                f"tg:album:{chat_id}:{album_id}:"
                f"{min(update_ids)}-{max(update_ids)}"
            )
        else:
            event_id = f"tg:{update_ids[0]}:{kind.value}"
        return BridgeEvent(
            event_id=event_id,
            platform=Platform.TELEGRAM,
            kind=kind,
            message_id=message_ids[0],
            message_ids=message_ids,
            author_id=str(sender.get("id", "")),
            author_name=author_name,
            route_id=route_id,
            author_username=sender.get("username"),
            text=text,
            reply_to_id=str(reply["message_id"]) if reply.get("message_id") else None,
            reply_quote=reply_text[:240] if reply_text else None,
            created_at_ms=int(first.get("date", time.time())) * 1000,
            attachments=tuple(attachments),
            checkpoint_key="telegram_offset",
            checkpoint_value=checkpoint,
        )

    @staticmethod
    def _attachment_from_message(message: dict[str, Any]) -> Attachment | None:
        message_id = str(message.get("message_id", ""))
        if message.get("photo"):
            photo = max(message["photo"], key=lambda item: item.get("file_size", 0))
            return Attachment(
                file_id=photo["file_id"],
                file_name=f"photo_{photo.get('file_unique_id', message_id)}.jpg",
                mime_type="image/jpeg",
                size=photo.get("file_size"),
                source_message_id=message_id,
            )
        for key, default_mime, extension in (
            ("document", "application/octet-stream", "bin"),
            ("video", "video/mp4", "mp4"),
            ("audio", "audio/mpeg", "mp3"),
            ("voice", "audio/ogg", "ogg"),
            ("animation", "video/mp4", "mp4"),
        ):
            item = message.get(key)
            if not item:
                continue
            mime = item.get("mime_type") or default_mime
            name = item.get("file_name") or f"{key}_{item.get('file_unique_id', message_id)}.{extension}"
            return Attachment(
                file_id=item["file_id"],
                file_name=safe_filename(name),
                mime_type=mime,
                size=item.get("file_size"),
                source_message_id=message_id,
            )
        return None

    async def fetch_author_avatar(
        self, event: BridgeEvent, max_bytes: int
    ) -> MaterializedAttachment | None:
        if event.platform is not Platform.TELEGRAM or not event.author_id.isdigit():
            return None
        result = await self.request(
            "getUserProfilePhotos",
            data={"user_id": event.author_id, "offset": "0", "limit": "1"},
            timeout=15,
        )
        photos = list(result.get("photos") or [])
        if not photos or not photos[0]:
            return None
        smallest = min(
            photos[0],
            key=lambda item: int(item.get("width", 0)) * int(item.get("height", 0)),
        )
        return await self.fetch_attachment(
            Attachment(
                file_id=str(smallest["file_id"]),
                file_name=f"telegram_avatar_{event.author_id}.jpg",
                mime_type="image/jpeg",
                size=smallest.get("file_size"),
            ),
            max_bytes,
        )

    async def fetch_attachment(
        self, attachment: Attachment, max_bytes: int
    ) -> MaterializedAttachment:
        if attachment.size is not None and attachment.size > max_bytes:
            raise AttachmentRejected(f"файл {attachment.file_name} превышает допустимый размер")
        info = await self.request("getFile", data={"file_id": attachment.file_id}, timeout=30)
        file_path = info["file_path"]
        try:
            async with self._session.get(
                f"{self._file_base}/{file_path}",
                timeout=aiohttp.ClientTimeout(total=60),
            ) as response:
                if response.status >= 500:
                    raise DeliveryError(f"Telegram file HTTP {response.status}", retryable=True)
                if response.status >= 400:
                    raise DeliveryError(f"Telegram file HTTP {response.status}", retryable=False)
                if response.content_length and response.content_length > max_bytes:
                    raise AttachmentRejected(
                        f"файл {attachment.file_name} превышает допустимый размер"
                    )
                data = await response.read()
        except (TimeoutError, aiohttp.ClientError) as error:
            raise DeliveryError(type(error).__name__, retryable=True) from error
        if len(data) > max_bytes:
            raise AttachmentRejected(f"файл {attachment.file_name} превышает допустимый размер")
        return MaterializedAttachment(
            file_name=safe_filename(attachment.file_name),
            mime_type=attachment.mime_type,
            data=data,
            source_message_id=attachment.source_message_id,
        )

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
    ) -> None:
        chat_id = self._chat_id_for(event.route_id)
        text = render_text(event, Platform.TELEGRAM, context.unknown_reply_quote)
        if attachment_warnings:
            text += "\n\n⚠️ " + "; ".join(attachment_warnings)
        parts: list[tuple[str, str | None, MaterializedAttachment | None]] = [
            ("text", chunk, None) for chunk in split_text(text, 4096)
        ]
        for attachment_item in attachments:
            method = (
                "sendPhoto"
                if attachment_item.mime_type.startswith("image/")
                else "sendDocument"
            )
            parts.append((method, None, attachment_item))

        for index, (kind, chunk, part_attachment) in enumerate(parts):
            if index < completed_parts:
                continue
            reply_to = context.reply_to_id or context.anchor_id
            if kind == "text":
                data: dict[str, Any] = {"chat_id": str(chat_id), "text": chunk or ""}
                if reply_to:
                    data["reply_parameters"] = json.dumps(
                        {
                            "message_id": int(reply_to),
                            "allow_sending_without_reply": True,
                        }
                    )
                result = await self.request("sendMessage", data=data)
            else:
                assert part_attachment is not None
                form = aiohttp.FormData()
                form.add_field("chat_id", str(chat_id))
                if reply_to:
                    form.add_field(
                        "reply_parameters",
                        json.dumps(
                            {
                                "message_id": int(reply_to),
                                "allow_sending_without_reply": True,
                            }
                        ),
                    )
                field_name = "photo" if kind == "sendPhoto" else "document"
                form.add_field(
                    field_name,
                    part_attachment.data,
                    filename=part_attachment.file_name,
                    content_type=part_attachment.mime_type
                    or mimetypes.guess_type(part_attachment.file_name)[0]
                    or "application/octet-stream",
                )
                result = await self.request(kind, data=form, timeout=90)
            await on_sent(SentMessage(message_id=str(result["message_id"]), part_index=index))

    async def send_failure_notice(self, event: BridgeEvent, reason: str) -> None:
        chat_id = self._chat_id_for(event.route_id)
        reason = reason.replace("\n", " ")[:240]
        data: dict[str, Any] = {
            "chat_id": str(chat_id),
            "text": f"⚠️ Сообщение не доставлено в Mattermost: {reason}",
        }
        if event.message_id.isdigit():
            data["reply_parameters"] = json.dumps(
                {
                    "message_id": int(event.message_id),
                    "allow_sending_without_reply": True,
                }
            )
        await self.request("sendMessage", data=data)

    async def edit(self, event: BridgeEvent, target_message_id: str) -> None:
        chat_id = self._chat_id_for(event.route_id)
        text = render_text(
            replace(event, kind=EventKind.MESSAGE),
            Platform.TELEGRAM,
        )
        if len(text) > 4096:
            raise DeliveryError(
                "edited message exceeds Telegram text limit",
                retryable=False,
            )
        await self.request(
            "editMessageText",
            data={
                "chat_id": str(chat_id),
                "message_id": target_message_id,
                "text": text,
            },
            timeout=15,
        )

    async def acknowledge_delivery(self, event: BridgeEvent) -> None:
        # Telegram bots can set only one reaction per message.  Delivery status
        # must not overwrite a real reaction mirrored from Mattermost.
        return

    async def sync_reactions(
        self,
        event: BridgeEvent,
        target_message_id: str,
        reactions: tuple[str, ...],
    ) -> None:
        chat_id = self._chat_id_for(event.route_id)
        selected = reactions[-1:]
        reaction_payload = json.dumps(
            [{"type": "emoji", "emoji": emoji} for emoji in selected],
            ensure_ascii=False,
            separators=(",", ":"),
        )
        await self.request(
            "setMessageReaction",
            data={
                "chat_id": str(chat_id),
                "message_id": target_message_id,
                "reaction": reaction_payload,
            },
            timeout=15,
        )
