from __future__ import annotations

import asyncio
from dataclasses import replace
from pathlib import Path

import pytest

from bridge.config import BridgeRoute
from bridge.core import Bridge
from bridge.models import (
    BridgeEvent,
    DeliveryContext,
    DeliveryError,
    EventKind,
    MaterializedAttachment,
    Platform,
    SentMessage,
)
from bridge.storage import Storage


class FakeAdapter:
    def __init__(self, platform: Platform) -> None:
        self.platform = platform
        self.sent: list[tuple[BridgeEvent, DeliveryContext]] = []
        self.sent_avatars: list[MaterializedAttachment | None] = []
        self.edited: list[tuple[BridgeEvent, str]] = []
        self.author_avatar: MaterializedAttachment | None = None
        self.acknowledged: list[str] = []
        self.acknowledged_event = asyncio.Event()
        self.notices: list[str] = []
        self.reactions: list[tuple[str, tuple[str, ...]]] = []
        self.next_id = 1000

    async def fetch_attachment(self, attachment, max_bytes: int) -> MaterializedAttachment:
        return MaterializedAttachment(attachment.file_name, attachment.mime_type, b"data")

    async def fetch_author_avatar(
        self, event: BridgeEvent, max_bytes: int
    ) -> MaterializedAttachment | None:
        return self.author_avatar

    async def send(
        self,
        event,
        context,
        attachments,
        attachment_warnings,
        completed_parts,
        on_sent,
        *,
        author_avatar=None,
    ) -> None:
        self.sent.append((event, context))
        self.sent_avatars.append(author_avatar)
        message_id = str(self.next_id)
        self.next_id += 1
        await on_sent(
            SentMessage(
                message_id=message_id,
                root_id=message_id if self.platform is Platform.MATTERMOST else None,
                part_index=0,
            )
        )

    async def send_failure_notice(self, event, reason: str) -> None:
        self.notices.append(reason)

    async def edit(self, event: BridgeEvent, target_message_id: str) -> None:
        self.edited.append((event, target_message_id))

    async def acknowledge_delivery(self, event: BridgeEvent) -> None:
        self.acknowledged.append(event.message_id)
        self.acknowledged_event.set()

    async def sync_reactions(
        self,
        event: BridgeEvent,
        target_message_id: str,
        reactions: tuple[str, ...],
    ) -> None:
        self.reactions.append((target_message_id, reactions))


class FailingAdapter(FakeAdapter):
    def __init__(self, platform: Platform, *, retryable: bool) -> None:
        super().__init__(platform)
        self.retryable = retryable

    async def send(self, *args, **kwargs) -> None:
        raise DeliveryError("simulated failure", retryable=self.retryable, retry_after=1)


def tg_event(message_id: str, *, reply_to: str | None = None) -> BridgeEvent:
    return BridgeEvent(
        event_id=f"tg:{message_id}",
        platform=Platform.TELEGRAM,
        kind=EventKind.MESSAGE,
        message_id=message_id,
        message_ids=(message_id,),
        author_id="tg-user",
        author_name="Contractor",
        text="hello",
        reply_to_id=reply_to,
    )


def mm_event(message_id: str, *, root_id: str | None = None) -> BridgeEvent:
    return BridgeEvent(
        event_id=f"mm:{message_id}",
        platform=Platform.MATTERMOST,
        kind=EventKind.MESSAGE,
        message_id=message_id,
        message_ids=(message_id,),
        author_id="mm-user",
        author_name="Employee",
        text="answer",
        root_id=root_id,
    )


@pytest.mark.asyncio
async def test_bidirectional_thread_mapping(tmp_path: Path) -> None:
    storage = Storage(tmp_path / "bridge.db")
    storage.migrate()
    telegram = FakeAdapter(Platform.TELEGRAM)
    mattermost = FakeAdapter(Platform.MATTERMOST)
    bridge = Bridge(
        storage,
        {Platform.TELEGRAM: telegram, Platform.MATTERMOST: mattermost},
        tg_chat_id=-1001,
        max_attachment_bytes=1024,
    )

    incoming = tg_event("10")
    assert await bridge.accept(incoming)
    job = storage.fetch_due_job()
    assert job is not None
    await bridge._deliver(job)
    root_link = storage.find_by_tg(-1001, "10")
    assert root_link is not None
    assert root_link.mm_root_id == "1000"
    assert root_link.tg_anchor_message_id == "10"

    reply = mm_event("mm-reply", root_id=root_link.mm_root_id)
    assert await bridge.accept(reply)
    job = storage.fetch_due_job()
    assert job is not None
    await bridge._deliver(job)
    assert telegram.sent[-1][1].reply_to_id == "10"

    contractor_reply = tg_event("11", reply_to="10")
    assert await bridge.accept(contractor_reply)
    job = storage.fetch_due_job()
    assert job is not None
    await bridge._deliver(job)
    assert mattermost.sent[-1][1].root_id == root_link.mm_root_id
    storage.close()


@pytest.mark.asyncio
async def test_unknown_telegram_reply_becomes_new_root_with_quote(tmp_path: Path) -> None:
    storage = Storage(tmp_path / "bridge.db")
    storage.migrate()
    telegram = FakeAdapter(Platform.TELEGRAM)
    mattermost = FakeAdapter(Platform.MATTERMOST)
    bridge = Bridge(
        storage,
        {Platform.TELEGRAM: telegram, Platform.MATTERMOST: mattermost},
        tg_chat_id=-1001,
        max_attachment_bytes=1024,
    )
    value = tg_event("20", reply_to="999")
    value = replace(value, reply_quote="old message")
    await bridge.accept(value)
    job = storage.fetch_due_job()
    assert job is not None
    await bridge._deliver(job)
    context = mattermost.sent[-1][1]
    assert context.root_id is None
    assert context.unknown_reply_quote == "old message"
    storage.close()


@pytest.mark.asyncio
async def test_telegram_edit_updates_linked_mattermost_post_without_new_post(
    tmp_path: Path,
) -> None:
    storage = Storage(tmp_path / "bridge.db")
    storage.migrate()
    telegram = FakeAdapter(Platform.TELEGRAM)
    mattermost = FakeAdapter(Platform.MATTERMOST)
    bridge = Bridge(
        storage,
        {Platform.TELEGRAM: telegram, Platform.MATTERMOST: mattermost},
        tg_chat_id=-1001,
        max_attachment_bytes=1024,
    )

    await bridge.accept(tg_event("40"))
    initial_job = storage.fetch_due_job()
    assert initial_job is not None
    await bridge._deliver(initial_job)

    edited = replace(
        tg_event("40"),
        event_id="tg:41:edit",
        kind=EventKind.EDIT,
        text="updated",
    )
    await bridge.accept(edited)
    edit_job = storage.fetch_due_job()
    assert edit_job is not None
    await bridge._deliver(edit_job)

    assert [(event.text, target_id) for event, target_id in mattermost.edited] == [
        ("updated", "1000")
    ]
    assert len(mattermost.sent) == 1
    storage.close()


@pytest.mark.asyncio
async def test_mattermost_reply_edit_updates_exact_telegram_reply_without_new_message(
    tmp_path: Path,
) -> None:
    storage = Storage(tmp_path / "bridge.db")
    storage.migrate()
    telegram = FakeAdapter(Platform.TELEGRAM)
    mattermost = FakeAdapter(Platform.MATTERMOST)
    bridge = Bridge(
        storage,
        {Platform.TELEGRAM: telegram, Platform.MATTERMOST: mattermost},
        tg_chat_id=-1001,
        max_attachment_bytes=1024,
    )

    await bridge.accept(tg_event("10"))
    root_job = storage.fetch_due_job()
    assert root_job is not None
    await bridge._deliver(root_job)

    reply = mm_event("mm-reply", root_id="1000")
    await bridge.accept(reply)
    reply_job = storage.fetch_due_job()
    assert reply_job is not None
    await bridge._deliver(reply_job)

    edited = replace(
        reply,
        event_id="mm:mm-reply:edit:2",
        kind=EventKind.EDIT,
        text="updated answer",
    )
    await bridge.accept(edited)
    edit_job = storage.fetch_due_job()
    assert edit_job is not None
    await bridge._deliver(edit_job)

    assert [(event.text, target_id) for event, target_id in telegram.edited] == [
        ("updated answer", "1000")
    ]
    assert len(telegram.sent) == 1
    storage.close()


@pytest.mark.asyncio
async def test_multiple_routes_keep_telegram_message_links_isolated(tmp_path: Path) -> None:
    storage = Storage(tmp_path / "bridge.db")
    storage.migrate()
    telegram = FakeAdapter(Platform.TELEGRAM)
    mattermost = FakeAdapter(Platform.MATTERMOST)
    bridge = Bridge(
        storage,
        {Platform.TELEGRAM: telegram, Platform.MATTERMOST: mattermost},
        routes={
            "a": BridgeRoute("a", -1001, "channel-a"),
            "b": BridgeRoute("b", -1002, "channel-b"),
        },
        max_attachment_bytes=1024,
    )

    for route_id in ("a", "b"):
        value = replace(
            tg_event("10"),
            event_id=f"tg:{route_id}:message",
            route_id=route_id,
        )
        await bridge.accept(value)
        job = storage.fetch_due_job()
        assert job is not None
        await bridge._deliver(job)

    link_a = storage.find_by_tg(-1001, "10")
    link_b = storage.find_by_tg(-1002, "10")
    assert link_a is not None and link_a.mm_post_id == "1000"
    assert link_b is not None and link_b.mm_post_id == "1001"
    storage.close()


@pytest.mark.asyncio
async def test_successful_delivery_acknowledges_source_message(tmp_path: Path) -> None:
    storage = Storage(tmp_path / "bridge.db")
    storage.migrate()
    telegram = FakeAdapter(Platform.TELEGRAM)
    mattermost = FakeAdapter(Platform.MATTERMOST)
    bridge = Bridge(
        storage,
        {Platform.TELEGRAM: telegram, Platform.MATTERMOST: mattermost},
        tg_chat_id=-1001,
        max_attachment_bytes=1024,
    )
    await bridge.accept(tg_event("40"))
    stop = asyncio.Event()
    worker = asyncio.create_task(bridge.run_outbox(stop))
    try:
        await asyncio.wait_for(telegram.acknowledged_event.wait(), timeout=0.5)
    finally:
        stop.set()
        await worker
        storage.close()
    assert telegram.acknowledged == ["40"]


@pytest.mark.asyncio
async def test_telegram_avatar_reaches_mattermost_delivery(tmp_path: Path) -> None:
    storage = Storage(tmp_path / "bridge.db")
    storage.migrate()
    telegram = FakeAdapter(Platform.TELEGRAM)
    telegram.author_avatar = MaterializedAttachment("avatar.jpg", "image/jpeg", b"JPEG")
    mattermost = FakeAdapter(Platform.MATTERMOST)
    bridge = Bridge(
        storage,
        {Platform.TELEGRAM: telegram, Platform.MATTERMOST: mattermost},
        tg_chat_id=-1001,
        max_attachment_bytes=1024,
    )
    await bridge.accept(tg_event("50"))
    stop = asyncio.Event()
    worker = asyncio.create_task(bridge.run_outbox(stop))
    try:
        await asyncio.wait_for(telegram.acknowledged_event.wait(), timeout=0.5)
    finally:
        stop.set()
        await worker
        storage.close()
    assert mattermost.sent_avatars == [telegram.author_avatar]


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("retryable", "expected_state", "notice_count"),
    [(True, "retry", 0), (False, "dead", 1)],
)
async def test_delivery_error_retry_and_dead_letter(
    tmp_path: Path, retryable: bool, expected_state: str, notice_count: int
) -> None:
    storage = Storage(tmp_path / f"bridge-{retryable}.db")
    storage.migrate()
    telegram = FakeAdapter(Platform.TELEGRAM)
    mattermost = FailingAdapter(Platform.MATTERMOST, retryable=retryable)
    bridge = Bridge(
        storage,
        {Platform.TELEGRAM: telegram, Platform.MATTERMOST: mattermost},
        tg_chat_id=-1001,
        max_attachment_bytes=1024,
    )
    await bridge.accept(tg_event("30"))
    job = storage.fetch_due_job()
    assert job is not None
    await bridge._deliver(job)
    assert storage.counts()[expected_state] == 1
    assert len(telegram.notices) == notice_count
    storage.close()


@pytest.mark.asyncio
async def test_reactions_are_aggregated_and_synced_to_linked_message(tmp_path: Path) -> None:
    storage = Storage(tmp_path / "bridge.db")
    storage.migrate()
    telegram = FakeAdapter(Platform.TELEGRAM)
    mattermost = FakeAdapter(Platform.MATTERMOST)
    bridge = Bridge(
        storage,
        {Platform.TELEGRAM: telegram, Platform.MATTERMOST: mattermost},
        tg_chat_id=-1001,
        max_attachment_bytes=1024,
    )
    await bridge.accept(tg_event("40"))
    message_job = storage.fetch_due_job()
    assert message_job is not None
    await bridge._deliver(message_job)

    async def react(event_id: str, kind: EventKind, actor: str) -> None:
        event = BridgeEvent(
            event_id=event_id,
            platform=Platform.TELEGRAM,
            kind=kind,
            message_id="40",
            message_ids=("40",),
            author_id=actor,
            author_name=actor,
            reaction="👍",
        )
        await bridge.accept(event)
        job = storage.fetch_due_job()
        assert job is not None
        await bridge._deliver(job)

    await react("r1", EventKind.REACTION_ADDED, "a")
    await react("r2", EventKind.REACTION_ADDED, "b")
    await react("r3", EventKind.REACTION_REMOVED, "a")
    await react("r4", EventKind.REACTION_REMOVED, "b")

    assert mattermost.reactions == [
        ("1000", ("👍",)),
        ("1000", ("👍",)),
        ("1000", ("👍",)),
        ("1000", ()),
    ]
    assert len(mattermost.sent) == 1
    storage.close()
