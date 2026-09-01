import json
import logging

from bridge.logging_json import JsonFormatter


def test_json_formatter_redacts_secret_fields() -> None:
    record = logging.LogRecord("bridge", logging.INFO, "", 0, "event", (), None)
    record.bot_token = "must-not-leak"
    record.event_id = "safe"
    payload = json.loads(JsonFormatter().format(record))
    assert payload["bot_token"] == "[REDACTED]"
    assert payload["event_id"] == "safe"
