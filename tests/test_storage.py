from pathlib import Path

from bridge.models import BridgeEvent, EventKind, Platform
from bridge.storage import Storage


def make_event(checkpoint: str = "11") -> BridgeEvent:
    return BridgeEvent(
        event_id="tg:10:message",
        platform=Platform.TELEGRAM,
        kind=EventKind.MESSAGE,
        message_id="100",
        message_ids=("100",),
        author_id="u",
        author_name="User",
        text="hello",
        checkpoint_key="telegram_offset",
        checkpoint_value=checkpoint,
    )


def test_enqueue_is_durable_and_idempotent(tmp_path: Path) -> None:
    storage = Storage(tmp_path / "bridge.db")
    storage.migrate()
    assert storage.enqueue(make_event()) is True
    assert storage.enqueue(make_event("9")) is False
    assert storage.get_state("telegram_offset") == "11"
    job = storage.fetch_due_job()
    assert job is not None
    assert job.event_id == "tg:10:message"
    storage.close()

    reopened = Storage(tmp_path / "bridge.db")
    reopened.migrate()
    reopened.recover_delivering_jobs()
    recovered = reopened.fetch_due_job()
    assert recovered is not None
    assert recovered.event_id == job.event_id
    reopened.close()


def test_ignored_event_only_advances_checkpoint(tmp_path: Path) -> None:
    storage = Storage(tmp_path / "bridge.db")
    storage.migrate()
    ignored = BridgeEvent(
        event_id="tg:ignored:1",
        platform=Platform.TELEGRAM,
        kind=EventKind.IGNORED,
        message_id="1",
        message_ids=("1",),
        author_id="",
        author_name="ignored",
        checkpoint_key="telegram_offset",
        checkpoint_value="2",
    )
    assert storage.enqueue(ignored)
    assert storage.fetch_due_job() is None
    assert storage.get_state("telegram_offset") == "2"
    storage.close()


def test_initialize_records_independent_mattermost_route_checkpoints(tmp_path: Path) -> None:
    storage = Storage(tmp_path / "bridge.db")
    storage.migrate()

    storage.initialize(mm_checkpoint_ms=1234, route_ids=("a", "b"))

    assert storage.get_state("mattermost_checkpoint_ms:a") == "1234"
    assert storage.get_state("mattermost_checkpoint_ms:b") == "1234"
    storage.close()


def reaction_event(
    event_id: str,
    *,
    kind: EventKind,
    actor: str,
    reaction: str,
    created_at_ms: int,
) -> BridgeEvent:
    return BridgeEvent(
        event_id=event_id,
        platform=Platform.MATTERMOST,
        kind=kind,
        message_id="post1",
        message_ids=("post1",),
        author_id=actor,
        author_name=actor,
        reaction=reaction,
        created_at_ms=created_at_ms,
    )


def test_reaction_state_counts_actors_and_falls_back_to_previous_reaction(
    tmp_path: Path,
) -> None:
    storage = Storage(tmp_path / "bridge.db")
    storage.migrate()

    storage.enqueue(
        reaction_event(
            "r1", kind=EventKind.REACTION_ADDED, actor="a", reaction="👍", created_at_ms=1
        )
    )
    storage.enqueue(
        reaction_event(
            "r2", kind=EventKind.REACTION_ADDED, actor="b", reaction="👍", created_at_ms=2
        )
    )
    storage.enqueue(
        reaction_event(
            "r3", kind=EventKind.REACTION_ADDED, actor="c", reaction="❤️", created_at_ms=3
        )
    )

    assert storage.active_reactions(
        Platform.MATTERMOST, "default", "post1"
    ) == ("👍", "❤️")

    storage.enqueue(
        reaction_event(
            "r4",
            kind=EventKind.REACTION_REMOVED,
            actor="c",
            reaction="❤️",
            created_at_ms=4,
        )
    )
    assert storage.active_reactions(
        Platform.MATTERMOST, "default", "post1"
    ) == ("👍",)

    storage.enqueue(
        reaction_event(
            "r5",
            kind=EventKind.REACTION_REMOVED,
            actor="a",
            reaction="👍",
            created_at_ms=5,
        )
    )
    assert storage.active_reactions(
        Platform.MATTERMOST, "default", "post1"
    ) == ("👍",)
    storage.close()
