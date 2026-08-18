#!/usr/bin/env python3
"""Generate Compose credentials without overwriting existing files or printing secrets."""
import argparse
from pathlib import Path
import secrets
import shutil


def configure(directory: Path) -> None:
    # mkdir is exclusive: existing data, including symlinks, is never replaced.
    directory.mkdir(mode=0o700)
    try:
        password = secrets.token_hex(24)
        values = {
            "postgres_password": password,
            "api_token": secrets.token_hex(32),
            "database_url": (
                f"postgres://forge:{password}@postgres:5432/forge"
                "?sslmode=disable&pool_max_conns=8&pool_min_conns=1&connect_timeout=5"
            ),
        }
        for name, value in values.items():
            path = directory / name
            with path.open("x", encoding="utf-8") as output:
                output.write(value + "\n")
            # Files must be readable by the non-root container UID. The parent
            # directory is private to the host user and files are mounted read-only.
            path.chmod(0o444)
    except BaseException:
        shutil.rmtree(directory)
        raise


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", type=Path, default=Path(__file__).resolve().parent.parent / ".secrets")
    args = parser.parse_args()
    try:
        configure(args.directory)
    except FileExistsError:
        parser.exit(1, f"Refusing to replace existing secret directory: {args.directory}\n")
    print(f"Created credentials in {args.directory}. No containers were started.")
