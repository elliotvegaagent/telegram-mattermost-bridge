from __future__ import annotations

import asyncio
import base64
import json
import logging
from collections.abc import Awaitable, Callable, Mapping
from dataclasses import replace
from typing import Any
from urllib.parse import urlparse, urlunparse

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
from ..reactions import MATTERMOST_BY_EMOJI, mattermost_reaction
from ..text import author_label, render_text, safe_filename, split_text

HealthCallback = Callable[[bool], None]


class MattermostAdapter:
    platform = Platform.MATTERMOST

    def __init__(
        self,
        session: aiohttp.ClientSession,
        *,
        base_url: str,
        token: str,
        channel_id: str | None = None,
        routes: Mapping[str, str] | None = None,
        health_callback: HealthCallback | None = None,
    ) -> None:
        self._session = session
        self._base_url = base_url.rstrip("/")
        self._api_base = f"{self._base_url}/api/v4"
        self._token = token
        if routes is None:
            if channel_id is None:
                raise ValueError("channel_id or routes is required")
            routes = {"default": channel_id}
        elif channel_id is not None:
            raise ValueError("use either channel_id or routes")
        self._channel_ids = dict(routes)
        if not self._channel_ids:
            raise ValueError("at least one Mattermost route is required")
        self._routes_by_channel = {
            route_channel_id: route_id
            for route_id, route_channel_id in self._channel_ids.items()
        }
        self._channel_id = next(iter(self._channel_ids.values()))
        self._headers = {"Authorization": f"Bearer {token}", "Accept": "application/json"}
        self._health_callback = health_callback or (lambda healthy: None)
        self._users: dict[str, dict[str, Any]] = {}
        self._self_user_id: str | None = None
        self._client_config: dict[str, Any] | None = None
        self._log = logging.getLogger("bridge.mattermost")

    async def request(
        self,
        method: str,
        path: str,
        *,
        json_body: Any | None = None,
        data: Any | None = None,
        params: dict[str, str] | None = None,
        expect_json: bool = True,
        timeout: float = 60,
    ) -> Any:
        try:
            async with self._session.request(
                method,
                f"{self._api_base}{path}",
                headers=self._headers,
                json=json_body,
                data=data,
                params=params,
                timeout=aiohttp.ClientTimeout(total=timeout),
            ) as response:
                if response.status == 429:
                    retry_after = float(response.headers.get("Retry-After", "5"))
                    raise DeliveryError(
                        "Mattermost rate limit", retryable=True, retry_after=retry_after
                    )
                if response.status >= 500:
                    raise DeliveryError(f"Mattermost HTTP {response.status}", retryable=True)
                if response.status >= 400:
                    detail = (await response.text()).replace("\n", " ")[:300]
                    raise DeliveryError(
                        f"Mattermost HTTP {response.status}: {detail}", retryable=False
                    )
                result = await response.json(content_type=None) if expect_json else await response.read()
        except DeliveryError:
            self._health_callback(False)
            raise
        except (TimeoutError, aiohttp.ClientError) as error:
            self._health_callback(False)
            raise DeliveryError(type(error).__name__, retryable=True) from error
        self._health_callback(True)
        return result

    async def get_me(self) -> dict[str, Any]:
        user = await self.request("GET", "/users/me")
        self._self_user_id = str(user["id"])
        return user

    def _channel_id_for(self, route_id: str) -> str:
        try:
            return self._channel_ids[route_id]
        except KeyError as error:
            raise DeliveryError(
                f"unknown bridge route: {route_id}", retryable=False
            ) from error

    async def get_channel(self, route_id: str | None = None) -> dict[str, Any]:
        channel_id = self._channel_id if route_id is None else self._channel_id_for(route_id)
        return await self.request("GET", f"/channels/{channel_id}")

    async def get_channel_member(
        self, user_id: str, route_id: str | None = None
    ) -> dict[str, Any]:
        channel_id = self._channel_id if route_id is None else self._channel_id_for(route_id)
        return await self.request(
            "GET", f"/channels/{channel_id}/members/{user_id}"
        )

    async def get_client_config(self) -> dict[str, Any]:
        if self._client_config is None:
            self._client_config = await self.request(
                "GET", "/config/client", params={"format": "old"}
            )
        return self._client_config

    async def get_user(self, user_id: str) -> dict[str, Any]:
        if user_id not in self._users:
            self._users[user_id] = await self.request("GET", f"/users/{user_id}")
        return self._users[user_id]

    async def get_post(self, post_id: str) -> dict[str, Any]:
        return await self.request("GET", f"/posts/{post_id}")

    async def get_post_files(self, post_id: str) -> list[dict[str, Any]]:
        return await self.request("GET", f"/posts/{post_id}/files/info")

    async def catch_up(
        self, since_ms: int | Mapping[str, int]
    ) -> list[dict[str, Any]]:
        checkpoints = (
            dict(since_ms)
            if isinstance(since_ms, Mapping)
            else {route_id: since_ms for route_id in self._channel_ids}
        )
        payloads = await asyncio.gather(
            *(
                self.request(
                    "GET",
                    f"/channels/{channel_id}/posts",
                    params={"since": str(max(0, checkpoints[route_id]))},
                )
                for route_id, channel_id in self._channel_ids.items()
            )
        )
        posts_by_id: dict[str, dict[str, Any]] = {}
        for payload in payloads:
            for post in (payload.get("posts") or {}).values():
                posts_by_id[str(post.get("id"))] = post
        posts = list(posts_by_id.values())
        return sorted(posts, key=lambda post: (post.get("create_at", 0), post.get("id", "")))

    async def event_from_post(
        self,
        post: dict[str, Any],
        *,
        kind: EventKind,
        self_user_id: str,
    ) -> BridgeEvent:
        checkpoint = max(
            int(post.get("create_at", 0)),
            int(post.get("update_at", 0)),
            int(post.get("delete_at", 0)),
        )
        user_id = str(post.get("user_id", ""))
        route_id = self._routes_by_channel.get(str(post.get("channel_id", "")))
        ignored = (
            route_id is None
            or user_id == self_user_id
            or bool(post.get("type"))
        )
        if ignored:
            return BridgeEvent(
                event_id=f"mm:{post.get('id', checkpoint)}:ignored:{checkpoint}",
                platform=Platform.MATTERMOST,
                kind=EventKind.IGNORED,
                message_id=str(post.get("id", checkpoint)),
                message_ids=(str(post.get("id", checkpoint)),),
                author_id=user_id,
                author_name="ignored",
                route_id=route_id or "ignored",
                checkpoint_key=(
                    f"mattermost_checkpoint_ms:{route_id}" if route_id else None
                ),
                checkpoint_value=str(checkpoint),
            )

        assert route_id is not None
        try:
            user = await self.get_user(user_id)
        except DeliveryError:
            user = {"id": user_id, "username": user_id}
        if user.get("is_bot"):
            return BridgeEvent(
                event_id=f"mm:{post.get('id', checkpoint)}:ignored-bot:{checkpoint}",
                platform=Platform.MATTERMOST,
                kind=EventKind.IGNORED,
                message_id=str(post.get("id", checkpoint)),
                message_ids=(str(post.get("id", checkpoint)),),
                author_id=user_id,
                author_name="ignored",
                route_id=route_id,
                checkpoint_key=f"mattermost_checkpoint_ms:{route_id}",
                checkpoint_value=str(checkpoint),
            )
        full_name = " ".join(
            part.strip()
            for part in (user.get("first_name", ""), user.get("last_name", ""))
            if part.strip()
        )
        author_name = full_name or user.get("nickname") or user.get("username") or user_id

        file_infos = list((post.get("metadata") or {}).get("files") or [])
        if not file_infos and post.get("file_ids"):
            try:
                file_infos = await self.get_post_files(str(post["id"]))
            except DeliveryError:
                file_infos = []
        attachments = tuple(
            Attachment(
                file_id=str(info["id"]),
                file_name=safe_filename(info.get("name") or "attachment"),
                mime_type=info.get("mime_type") or "application/octet-stream",
                size=info.get("size"),
                source_message_id=str(post["id"]),
            )
            for info in file_infos
            if info.get("id")
        )
        event_suffix = {
            EventKind.MESSAGE: "message",
            EventKind.EDIT: f"edit:{post.get('edit_at') or post.get('update_at')}",
            EventKind.DELETE: f"delete:{post.get('delete_at') or post.get('update_at')}",
        }.get(kind, "ignored")
        return BridgeEvent(
            event_id=f"mm:{post['id']}:{event_suffix}",
            platform=Platform.MATTERMOST,
            kind=kind,
            message_id=str(post["id"]),
            message_ids=(str(post["id"]),),
            author_id=user_id,
            author_name=author_name,
            route_id=route_id,
            author_username=user.get("username"),
            text=str(post.get("message") or ""),
            root_id=str(post.get("root_id")) if post.get("root_id") else None,
            created_at_ms=int(post.get("create_at", checkpoint)),
            attachments=attachments if kind is EventKind.MESSAGE else (),
            checkpoint_key=f"mattermost_checkpoint_ms:{route_id}",
            checkpoint_value=str(checkpoint),
        )

    def event_from_reaction(
        self,
        reaction: dict[str, Any],
        *,
        kind: EventKind,
        channel_id: str,
        self_user_id: str,
    ) -> BridgeEvent:
        user_id = str(reaction.get("user_id", ""))
        post_id = str(reaction.get("post_id", ""))
        emoji_name = str(reaction.get("emoji_name", ""))
        canonical = mattermost_reaction(emoji_name)
        route_id = self._routes_by_channel.get(channel_id)
        created_at = int(reaction.get("create_at", 0))
        ignored = (
            route_id is None
            or user_id == self_user_id
            or not post_id
            or canonical is None
        )
        suffix = "ignored" if ignored else kind.value
        return BridgeEvent(
            event_id=(
                f"mm:{post_id or 'unknown'}:{suffix}:{user_id}:"
                f"{emoji_name}:{created_at}"
            ),
            platform=Platform.MATTERMOST,
            kind=EventKind.IGNORED if ignored else kind,
            message_id=post_id or "unknown",
            message_ids=(post_id or "unknown",),
            author_id=user_id,
            author_name=user_id if not ignored else "ignored",
            route_id=route_id or "ignored",
            reaction=canonical,
            created_at_ms=created_at,
        )

    async def run_websocket(
        self,
        *,
        initial_checkpoints: Mapping[str, int],
        self_user_id: str,
        accept: Callable[[BridgeEvent], Awaitable[bool]],
        stop: asyncio.Event,
    ) -> None:
        checkpoint_ref = {
            route_id: int(initial_checkpoints[route_id])
            for route_id in self._channel_ids
        }
        first_connection = True
        delay = 1.0
        while not stop.is_set():
            try:
                since = {
                    route_id: (
                        checkpoint
                        if first_connection
                        else max(0, checkpoint - 5 * 60 * 1000)
                    )
                    for route_id, checkpoint in checkpoint_ref.items()
                }
                recovered = await self.catch_up(since)
                if len(recovered) >= 1000:
                    self._log.warning("catchup_result_limit_reached")
                for post in recovered:
                    kind = self._kind_from_post(post)
                    event = await self.event_from_post(
                        post, kind=kind, self_user_id=self_user_id
                    )
                    await accept(event)
                    if event.checkpoint_value and event.route_id in checkpoint_ref:
                        checkpoint_ref[event.route_id] = max(
                            checkpoint_ref[event.route_id], int(event.checkpoint_value)
                        )
                first_connection = False
                await self._consume_socket(
                    checkpoint_ref=checkpoint_ref,
                    self_user_id=self_user_id,
                    accept=accept,
                    stop=stop,
                )
                delay = 1.0
            except (TimeoutError, DeliveryError, aiohttp.ClientError):
                self._health_callback(False)
                self._log.warning("websocket_retry")
                try:
                    await asyncio.wait_for(stop.wait(), timeout=delay)
                except TimeoutError:
                    pass
                delay = min(60.0, delay * 2)

    async def _consume_socket(
        self,
        *,
        checkpoint_ref: dict[str, int],
        self_user_id: str,
        accept: Callable[[BridgeEvent], Awaitable[bool]],
        stop: asyncio.Event,
    ) -> None:
        parsed = urlparse(self._base_url)
        ws_scheme = "wss" if parsed.scheme == "https" else "ws"
        ws_url = urlunparse((ws_scheme, parsed.netloc, "/api/v4/websocket", "", "", ""))
        async with self._session.ws_connect(ws_url, heartbeat=25) as socket:
            await socket.send_json(
                {
                    "seq": 1,
                    "action": "authentication_challenge",
                    "data": {"token": self._token},
                }
            )
            self._health_callback(True)
            while not stop.is_set():
                try:
                    message = await asyncio.wait_for(socket.receive(), timeout=35)
                except TimeoutError:
                    await socket.ping()
                    continue
                if message.type in (
                    aiohttp.WSMsgType.CLOSED,
                    aiohttp.WSMsgType.CLOSE,
                    aiohttp.WSMsgType.ERROR,
                ):
                    raise DeliveryError("Mattermost WebSocket closed", retryable=True)
                if message.type is not aiohttp.WSMsgType.TEXT:
                    continue
                payload = json.loads(message.data)
                event_name = payload.get("event")
                if event_name not in {
                    "posted",
                    "post_edited",
                    "post_deleted",
                    "reaction_added",
                    "reaction_removed",
                }:
                    continue
                if event_name in {"reaction_added", "reaction_removed"}:
                    raw_reaction = (payload.get("data") or {}).get("reaction")
                    reaction = (
                        json.loads(raw_reaction)
                        if isinstance(raw_reaction, str)
                        else raw_reaction
                    )
                    if not isinstance(reaction, dict):
                        continue
                    broadcast = payload.get("broadcast") or {}
                    channel_id = str(
                        broadcast.get("channel_id")
                        or (payload.get("data") or {}).get("channel_id")
                        or ""
                    )
                    normalized = self.event_from_reaction(
                        reaction,
                        kind=(
                            EventKind.REACTION_ADDED
                            if event_name == "reaction_added"
                            else EventKind.REACTION_REMOVED
                        ),
                        channel_id=channel_id,
                        self_user_id=self_user_id,
                    )
                    await accept(normalized)
                    continue
                raw_post = (payload.get("data") or {}).get("post")
                post = json.loads(raw_post) if isinstance(raw_post, str) else raw_post
                if not isinstance(post, dict):
                    continue
                kind = {
                    "posted": EventKind.MESSAGE,
                    "post_edited": EventKind.EDIT,
                    "post_deleted": EventKind.DELETE,
                }[event_name]
                normalized = await self.event_from_post(
                    post, kind=kind, self_user_id=self_user_id
                )
                await accept(normalized)
                if (
                    normalized.checkpoint_value
                    and normalized.route_id in checkpoint_ref
                ):
                    checkpoint_ref[normalized.route_id] = max(
                        checkpoint_ref[normalized.route_id],
                        int(normalized.checkpoint_value),
                    )

    @staticmethod
    def _kind_from_post(post: dict[str, Any]) -> EventKind:
        if int(post.get("delete_at", 0)):
            return EventKind.DELETE
        if int(post.get("edit_at", 0)) > int(post.get("create_at", 0)):
            return EventKind.EDIT
        return EventKind.MESSAGE

    async def fetch_attachment(
        self, attachment: Attachment, max_bytes: int
    ) -> MaterializedAttachment:
        if attachment.size is not None and attachment.size > max_bytes:
            raise AttachmentRejected(f"файл {attachment.file_name} превышает допустимый размер")
        data = await self.request(
            "GET", f"/files/{attachment.file_id}", expect_json=False, timeout=90
        )
        if len(data) > max_bytes:
            raise AttachmentRejected(f"файл {attachment.file_name} превышает допустимый размер")
        return MaterializedAttachment(
            file_name=safe_filename(attachment.file_name),
            mime_type=attachment.mime_type,
            data=data,
            source_message_id=attachment.source_message_id,
        )

    async def fetch_author_avatar(
        self, event: BridgeEvent, max_bytes: int
    ) -> MaterializedAttachment | None:
        return None

    async def _upload_files(
        self, attachments: list[MaterializedAttachment], channel_id: str
    ) -> list[str]:
        if not attachments:
            return []
        form = aiohttp.FormData()
        form.add_field("channel_id", channel_id)
        for item in attachments:
            form.add_field(
                "files",
                item.data,
                filename=item.file_name,
                content_type=item.mime_type or "application/octet-stream",
            )
        payload = await self.request("POST", "/files", data=form, timeout=120)
        return [str(info["id"]) for info in payload.get("file_infos", [])]

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
        channel_id = self._channel_id_for(event.route_id)
        props: dict[str, Any] = {"from_telegram_mattermost_bridge": True}
        username_overridden = False
        if event.platform is Platform.TELEGRAM:
            config = await self.get_client_config()
            username_overridden = (
                str(config.get("EnablePostUsernameOverride", "false")).lower() == "true"
            )
            icon_overridden = (
                str(config.get("EnablePostIconOverride", "false")).lower() == "true"
            )
            if username_overridden:
                props["override_username"] = author_label(event)
            if (
                icon_overridden
                and author_avatar
                and author_avatar.mime_type.startswith("image/")
            ):
                encoded_avatar = base64.b64encode(author_avatar.data).decode("ascii")
                props["override_icon_url"] = (
                    f"data:{author_avatar.mime_type};base64,{encoded_avatar}"
                )
        text = render_text(
            event,
            Platform.MATTERMOST,
            context.unknown_reply_quote,
            include_author=True,
        )
        if attachment_warnings:
            text += "\n\n⚠️ " + "; ".join(attachment_warnings)
        text_chunks = split_text(text, 16_000)
        file_groups = [attachments[index : index + 5] for index in range(0, len(attachments), 5)]
        parts: list[tuple[str, list[MaterializedAttachment]]] = []
        if file_groups:
            parts.append((text_chunks[0], file_groups[0]))
            parts.extend(("📎 Вложения (продолжение)", group) for group in file_groups[1:])
            parts.extend((chunk, []) for chunk in text_chunks[1:])
        else:
            parts.extend((chunk, []) for chunk in text_chunks)

        root_id = context.root_id
        for index, (chunk, files) in enumerate(parts):
            if index < completed_parts:
                continue
            file_ids = await self._upload_files(files, channel_id)
            body: dict[str, Any] = {
                "channel_id": channel_id,
                "message": chunk,
                "props": props,
            }
            if root_id:
                body["root_id"] = root_id
            if file_ids:
                body["file_ids"] = file_ids
            result = await self.request("POST", "/posts", json_body=body)
            if root_id is None:
                root_id = str(result["id"])
            await on_sent(
                SentMessage(
                    message_id=str(result["id"]),
                    root_id=root_id,
                    part_index=index,
                )
            )

    async def send_failure_notice(self, event: BridgeEvent, reason: str) -> None:
        channel_id = self._channel_id_for(event.route_id)
        reason = reason.replace("\n", " ")[:240]
        root_id = event.root_id or event.message_id
        await self.request(
            "POST",
            "/posts",
            json_body={
                "channel_id": channel_id,
                "root_id": root_id,
                "message": f"⚠️ Сообщение не доставлено в Telegram: {reason}",
                "props": {"from_telegram_mattermost_bridge": True},
            },
        )

    async def edit(self, event: BridgeEvent, target_message_id: str) -> None:
        message = render_text(
            replace(event, kind=EventKind.MESSAGE),
            Platform.MATTERMOST,
            include_author=True,
        )
        await self.request(
            "PUT",
            f"/posts/{target_message_id}/patch",
            json_body={"message": message},
        )

    async def acknowledge_delivery(self, event: BridgeEvent) -> None:
        return

    async def sync_reactions(
        self,
        event: BridgeEvent,
        target_message_id: str,
        reactions: tuple[str, ...],
    ) -> None:
        if self._self_user_id is None:
            await self.get_me()
        assert self._self_user_id is not None
        current_payload = await self.request(
            "GET", f"/posts/{target_message_id}/reactions"
        )
        managed_names = set(MATTERMOST_BY_EMOJI.values())
        current = {
            str(item.get("emoji_name"))
            for item in (current_payload or [])
            if str(item.get("user_id")) == self._self_user_id
            and str(item.get("emoji_name")) in managed_names
        }
        desired = {
            MATTERMOST_BY_EMOJI[reaction]
            for reaction in reactions
            if reaction in MATTERMOST_BY_EMOJI
        }
        for emoji_name in sorted(desired - current):
            await self.request(
                "POST",
                "/reactions",
                json_body={
                    "user_id": self._self_user_id,
                    "post_id": target_message_id,
                    "emoji_name": emoji_name,
                },
            )
        for emoji_name in sorted(current - desired):
            await self.request(
                "DELETE",
                f"/users/{self._self_user_id}/posts/{target_message_id}"
                f"/reactions/{emoji_name}",
            )
