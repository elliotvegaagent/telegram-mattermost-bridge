from bridge.models import BridgeEvent, EventKind, Platform
from bridge.text import author_label, render_text, split_text


def event(text: str) -> BridgeEvent:
    return BridgeEvent(
        event_id="tg:1",
        platform=Platform.TELEGRAM,
        kind=EventKind.MESSAGE,
        message_id="10",
        message_ids=("10",),
        author_id="20",
        author_name="Contractor",
        author_username="external",
        text=text,
    )


def test_external_mattermost_text_cannot_trigger_mentions_or_markdown() -> None:
    result = render_text(event("@channel **urgent** [link](https://example.test)"), Platform.MATTERMOST)
    assert "@\u200bchannel" in result
    assert r"\*\*urgent\*\*" in result
    assert r"\[link\]\(https://example\.test\)" in result


def test_author_label_has_stable_identity_marker() -> None:
    assert author_label(event("hello")) == "🟠 Contractor · Telegram (@external)"


def test_mattermost_message_is_compact_in_telegram() -> None:
    value = BridgeEvent(
        event_id="mm:1",
        platform=Platform.MATTERMOST,
        kind=EventKind.MESSAGE,
        message_id="post-1",
        message_ids=("post-1",),
        author_id="user-1",
        author_name="Дмитрий Карнаухов",
        author_username="dkarnaukhov",
        text="все понятно",
    )
    assert render_text(value, Platform.TELEGRAM) == "Дмитрий Карнаухов : все понятно"


def test_split_text_is_bounded_and_numbered() -> None:
    chunks = split_text("word " * 100, 80)
    assert len(chunks) > 1
    assert all(len(chunk) <= 80 for chunk in chunks)
    assert chunks[0].startswith("(1/")
