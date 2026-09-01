from __future__ import annotations

import asyncio
import logging
from collections.abc import Mapping

from .config import BridgeRoute
from .models import (
    BridgeEvent,
    DeliveryContext,
    DeliveryError,
    EventKind,
    MessageLink,
    OutboxJob,
    Platform,
    SentMessage,
)
from .ports import AttachmentRejected, PlatformAdapter
from .storage import Storage


class Bridge:
    """Deep module that owns durability, routing, retries, and thread correlation."""

    def __init__(
        self,
        storage: Storage,
        adapters: Mapping[Platform, PlatformAdapter],
        *,
        tg_chat_id: int | None = None,
        routes: Mapping[str, BridgeRoute] | None = None,
        max_attachment_bytes: int,
    ) -> None:
        self._storage = storage
        self._adapters = dict(adapters)
        if routes is None:
            if tg_chat_id is None:
                raise ValueError("tg_chat_id or routes is required")
            routes = {"default": BridgeRoute("default", tg_chat_id, "")}
        elif tg_chat_id is not None:
            raise ValueError("use either tg_chat_id or routes")
        self._routes = dict(routes)
        if not self._routes:
            raise ValueError("at least one bridge route is required")
        self._max_attachment_bytes = max_attachment_bytes
        self._log = logging.getLogger("bridge.core")

    def _tg_chat_id_for(self, event: BridgeEvent) -> int:
        try:
            return self._routes[event.route_id].tg_chat_id
        except KeyError as error:
            raise DeliveryError(
                f"unknown bridge route: {event.route_id}", retryable=False
            ) from error

    async def accept(self, event: BridgeEvent) -> bool:
        """Persist one normalized event and its source checkpoint atomically."""
        inserted = self._storage.enqueue(event)
        self._log.info(
            "event_accepted",
            extra={
                "event_id": event.event_id,
                "source": event.platform.value,
                "kind": event.kind.value,
                "duplicate": not inserted,
            },
        )
        return inserted

    async def run_outbox(self, stop: asyncio.Event) -> None:
        self._storage.recover_delivering_jobs()
        while not stop.is_set():
            job = self._storage.fetch_due_job()
            if job is None:
                try:
                    await asyncio.wait_for(stop.wait(), timeout=0.5)
                except TimeoutError:
                    pass
                continue
            await self._deliver(job)

    async def _deliver(self, job: OutboxJob) -> None:
        try:
            event = self._storage.get_event(job.event_id)
            context = self._resolve_context(event)
            source = self._adapters[event.platform]
            target = self._adapters[job.target]

            if event.kind in {
                EventKind.REACTION_ADDED,
                EventKind.REACTION_REMOVED,
            }:
                target_message_id = self._reaction_target_id(event)
                if target_message_id is not None:
                    reactions = self._storage.active_reactions(
                        event.platform,
                        event.route_id,
                        event.message_id,
                    )
                    if job.target is Platform.TELEGRAM:
                        reactions = reactions[-1:]
                    await target.sync_reactions(event, target_message_id, reactions)
                self._storage.complete_job(job.id, event.event_id)
                self._log.info(
                    "reactions_synchronized",
                    extra={"event_id": event.event_id, "target": job.target.value},
                )
                return

            if event.kind is EventKind.EDIT and context.edit_target_id:
                await target.edit(event, context.edit_target_id)
                self._storage.complete_job(job.id, event.event_id)
                self._log.info(
                    "delivery_edited",
                    extra={"event_id": event.event_id, "target": job.target.value},
                )
                return

            author_avatar = None
            try:
                author_avatar = await source.fetch_author_avatar(
                    event,
                    min(self._max_attachment_bytes, 128 * 1024),
                )
            except Exception:  # noqa: BLE001 - avatar is optional presentation.
                self._log.warning(
                    "author_avatar_unavailable",
                    extra={"event_id": event.event_id, "source": event.platform.value},
                )

            attachments = []
            warnings: list[str] = []
            for item in event.attachments:
                try:
                    attachments.append(
                        await source.fetch_attachment(item, self._max_attachment_bytes)
                    )
                except AttachmentRejected as error:
                    warnings.append(str(error))

            completed_parts = int(job.progress.get("completed_parts", 0))
            link_state = {
                "mm_root": context.root_id,
                "tg_anchor": context.anchor_id,
            }

            async def on_sent(sent: SentMessage) -> None:
                self._record_link(event, sent, link_state)
                job.progress["completed_parts"] = sent.part_index + 1
                self._storage.update_progress(job.id, job.progress)

            await target.send(
                event,
                context,
                attachments,
                warnings,
                completed_parts,
                on_sent,
                author_avatar=author_avatar,
            )
            self._storage.complete_job(job.id, event.event_id)
            try:
                await source.acknowledge_delivery(event)
            except Exception:  # noqa: BLE001 - acknowledgement must not undo delivery.
                self._log.warning(
                    "delivery_acknowledgement_failed",
                    extra={"event_id": event.event_id, "source": event.platform.value},
                )
            self._log.info(
                "delivery_completed",
                extra={"event_id": event.event_id, "target": job.target.value},
            )
        except DeliveryError as error:
            if error.retryable:
                default_delay = min(300.0, 5.0 * (2 ** min(job.attempts, 6)))
                self._storage.retry_job(job, str(error), error.retry_after or default_delay)
                self._log.warning(
                    "delivery_retry",
                    extra={"event_id": job.event_id, "target": job.target.value},
                )
            else:
                self._storage.dead_letter_job(job, str(error))
                await self._notify_failure(job, str(error))
        except Exception as error:  # noqa: BLE001 - preserve every durable event.
            delay = min(300.0, 5.0 * (2 ** min(job.attempts, 6)))
            self._storage.retry_job(job, type(error).__name__, delay)
            self._log.exception(
                "delivery_unexpected_error",
                extra={"event_id": job.event_id, "target": job.target.value},
            )

    async def _notify_failure(self, job: OutboxJob, reason: str) -> None:
        try:
            event = self._storage.get_event(job.event_id)
            await self._adapters[event.platform].send_failure_notice(event, reason)
        except Exception:  # noqa: BLE001 - notice failure cannot fail the worker.
            self._log.exception("failure_notice_failed", extra={"event_id": job.event_id})

    def _resolve_context(self, event: BridgeEvent) -> DeliveryContext:
        tg_chat_id = self._tg_chat_id_for(event)
        if event.platform is Platform.TELEGRAM:
            existing = self._storage.find_by_tg(tg_chat_id, event.message_ids[0])
            linked = existing
            if linked is None and event.reply_to_id:
                linked = self._storage.find_by_tg(tg_chat_id, event.reply_to_id)
            return DeliveryContext(
                reply_to_id=linked.mm_post_id if linked else None,
                root_id=linked.mm_root_id if linked else None,
                anchor_id=linked.tg_anchor_message_id if linked else None,
                unknown_reply_quote=event.reply_quote if event.reply_to_id and not linked else None,
                edit_target_id=existing.mm_post_id if existing else None,
            )

        existing = self._storage.find_by_mm(event.message_id)
        linked = existing
        if linked is None and event.root_id:
            linked = self._storage.find_by_mm_root(event.root_id)
        return DeliveryContext(
            reply_to_id=linked.tg_anchor_message_id if linked else None,
            root_id=linked.mm_root_id if linked else event.root_id,
            anchor_id=linked.tg_anchor_message_id if linked else None,
            edit_target_id=existing.tg_message_id if existing else None,
        )

    def _reaction_target_id(self, event: BridgeEvent) -> str | None:
        tg_chat_id = self._tg_chat_id_for(event)
        if event.platform is Platform.TELEGRAM:
            link = self._storage.find_by_tg(tg_chat_id, event.message_id)
            return link.mm_post_id if link else None
        link = self._storage.find_by_mm(event.message_id)
        return link.tg_message_id if link else None

    def _record_link(
        self,
        event: BridgeEvent,
        sent: SentMessage,
        state: dict[str, str | None],
    ) -> None:
        tg_chat_id = self._tg_chat_id_for(event)
        if event.platform is Platform.TELEGRAM:
            mm_root = state["mm_root"] or sent.root_id or sent.message_id
            tg_anchor = state["tg_anchor"] or event.message_ids[0]
            state["mm_root"] = mm_root
            state["tg_anchor"] = tg_anchor
            for tg_message_id in event.message_ids:
                self._storage.add_link(
                    MessageLink(
                        mm_post_id=sent.message_id,
                        tg_chat_id=tg_chat_id,
                        tg_message_id=tg_message_id,
                        mm_root_id=mm_root,
                        tg_anchor_message_id=tg_anchor,
                        part_index=sent.part_index,
                    )
                )
            return

        mm_root = event.root_id or event.message_id
        tg_anchor = state["tg_anchor"] or sent.message_id
        state["tg_anchor"] = tg_anchor
        self._storage.add_link(
            MessageLink(
                mm_post_id=event.message_id,
                tg_chat_id=tg_chat_id,
                tg_message_id=sent.message_id,
                mm_root_id=mm_root,
                tg_anchor_message_id=tg_anchor,
                part_index=sent.part_index,
            )
        )
