import json

from bridge.config import Settings


def test_loads_multiple_bridge_pairs(monkeypatch) -> None:
    monkeypatch.setenv("MM_URL", "https://mattermost.example.test")
    monkeypatch.setenv("MM_BOT_TOKEN", "mm-secret")
    monkeypatch.setenv("TG_BOT_TOKEN", "tg-secret")
    monkeypatch.setenv(
        "BRIDGE_PAIRS",
        json.dumps(
            [
                {"id": "a", "tg_chat_id": -1001, "mm_channel_id": "channel-a"},
                {"id": "b", "tg_chat_id": -1002, "mm_channel_id": "channel-b"},
            ]
        ),
    )
    monkeypatch.delenv("TG_CHAT_ID", raising=False)
    monkeypatch.delenv("MM_CHANNEL_ID", raising=False)

    settings = Settings.from_env()

    assert [
        (route.id, route.tg_chat_id, route.mm_channel_id)
        for route in settings.routes
    ] == [
        ("a", -1001, "channel-a"),
        ("b", -1002, "channel-b"),
    ]
