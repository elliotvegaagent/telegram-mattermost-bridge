from __future__ import annotations

from typing import Any

# Canonical bridge representation is the standard Unicode emoji.  Custom and
# paid reactions are deliberately outside the MVP because neither platform can
# reproduce them reliably on the other side.
MATTERMOST_BY_EMOJI: dict[str, str] = {
    "👍": "+1",
    "👎": "-1",
    "❤️": "heart",
    "🔥": "fire",
    "🎉": "tada",
    "✅": "white_check_mark",
    "👀": "eyes",
    "😂": "joy",
}
EMOJI_BY_MATTERMOST = {name: emoji for emoji, name in MATTERMOST_BY_EMOJI.items()}
SUPPORTED_REACTIONS = frozenset(MATTERMOST_BY_EMOJI)


def telegram_reaction(value: dict[str, Any]) -> str | None:
    if value.get("type") != "emoji":
        return None
    emoji = str(value.get("emoji") or "")
    return emoji if emoji in SUPPORTED_REACTIONS else None


def mattermost_reaction(emoji_name: str) -> str | None:
    return EMOJI_BY_MATTERMOST.get(emoji_name)
