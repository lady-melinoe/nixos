import sys

from .errors import ProvisionError
from .settings import DEFAULT_EXTENSIONS


def prompt(message: str) -> str:
    if not sys.stdin.isatty():
        raise ProvisionError(f"{message} is required (not running interactively to prompt for it).")
    print(f"{message}: ", end="", file=sys.stderr, flush=True)
    return input()


def prompt_default(message: str, default: str) -> str:
    if not sys.stdin.isatty():
        return default
    print(f"{message} [{default}]: ", end="", file=sys.stderr, flush=True)
    return input() or default


def prompt_yes_no(message: str, default: bool = False) -> bool:
    suffix = "Y/n" if default else "y/N"
    while True:
        print(f"{message} [{suffix}]: ", end="", file=sys.stderr, flush=True)
        value = input().strip().lower()
        if not value:
            return default
        if value in ("y", "yes"):
            return True
        if value in ("n", "no"):
            return False
        print("Please answer y or n.", file=sys.stderr)


def parse_extension_selection(value: str, extensions: list[str]) -> list[str]:
    """Parse n/y, numbers, comma-separated numbers and numeric ranges."""
    value = value.strip().lower()
    if value in ("", "n", "none"):
        return []
    if value in ("y", "yes", "all"):
        return list(extensions)

    selected: set[int] = set()
    for part in value.replace(" ", "").split(","):
        if not part:
            continue
        if "-" in part:
            start, _, end = part.partition("-")
            if not (start.isdigit() and end.isdigit()) or int(start) > int(end):
                raise ValueError(f"invalid range: {part}")
            selected.update(range(int(start), int(end) + 1))
        elif part.isdigit():
            selected.add(int(part))
        else:
            raise ValueError(f"invalid selection: {part}")

    for number in selected:
        if not 1 <= number <= len(extensions):
            raise ValueError(f"selection out of range: {number}")

    return [extensions[i - 1] for i in sorted(selected)]


def prompt_extensions(default: list[str]) -> list[str]:
    print("\nExtensions:", file=sys.stderr)
    for i, extension in enumerate(DEFAULT_EXTENSIONS, 1):
        print(f"  {i}. {extension}", file=sys.stderr)

    default_text = (
        ",".join(
            str(DEFAULT_EXTENSIONS.index(ext) + 1) for ext in default if ext in DEFAULT_EXTENSIONS
        )
        or "none"
    )

    while True:
        print(
            f"\nSelection [N=none, Y=all, default={default_text}]: ",
            end="",
            file=sys.stderr,
            flush=True,
        )
        value = input().strip()
        if not value:
            return list(default)
        try:
            return parse_extension_selection(value, DEFAULT_EXTENSIONS)
        except ValueError as e:
            print(f"Invalid selection: {e}", file=sys.stderr)


def prompt_forced_command(current: str | None = None) -> str | None:
    label = (
        "Forced command [N/<command>]"
        if current is None
        else f"Forced command [current: {current}, N/<command>]"
    )
    print(f"{label}: ", end="", file=sys.stderr, flush=True)
    value = input().strip()
    if not value:
        return current
    if value.lower() in ("n", "none"):
        return None
    return value
