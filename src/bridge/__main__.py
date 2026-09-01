from __future__ import annotations

import argparse
import asyncio
import sys

from .config import ConfigError, Settings
from .logging_json import configure_logging
from .runtime import doctor, initialize, print_result, run
from .storage import Storage


def parser() -> argparse.ArgumentParser:
    value = argparse.ArgumentParser(description="Telegram ↔ Mattermost bridge")
    subparsers = value.add_subparsers(dest="command", required=True)
    subparsers.add_parser("doctor", help="validate credentials, IDs, permissions and database")
    init = subparsers.add_parser("init", help="drop pending Telegram updates and start from now")
    init.add_argument(
        "--force", action="store_true", help="reset checkpoints when already initialized"
    )
    subparsers.add_parser("run", help="run the bridge")
    return value


async def async_main(args: argparse.Namespace, settings: Settings, storage: Storage) -> None:
    if args.command == "doctor":
        print_result(await doctor(settings, storage))
    elif args.command == "init":
        print_result(await initialize(settings, storage, force=args.force))
    else:
        await run(settings, storage)


def main() -> None:
    args = parser().parse_args()
    try:
        settings = Settings.from_env()
        configure_logging(settings.log_level)
        storage = Storage(settings.db_path)
        storage.migrate()
        try:
            asyncio.run(async_main(args, settings, storage))
        finally:
            storage.close()
    except (ConfigError, RuntimeError) as error:
        print(f"error: {error}", file=sys.stderr)
        raise SystemExit(2) from error


if __name__ == "__main__":
    main()
