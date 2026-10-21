import sys
from argparse import Namespace
from pathlib import Path

from . import keygen, prompts
from .agents import agents
from .errors import ProvisionError
from .settings import DEFAULT_EXTENSIONS, USER_CA_KEY


def read_key_material(identity: str | None) -> bytes:
    """A public key or certificate from a path, or from stdin for "-" or nothing."""
    if identity and identity != "-":
        path = Path(identity)
        if not path.is_file():
            raise ProvisionError(f"file not found: {identity}")
        return path.read_bytes()

    if sys.stdin.isatty():
        print("Paste the key, then press Ctrl-D:", file=sys.stderr)
    return sys.stdin.buffer.read()


def emit(cert: bytes, output: str | None) -> None:
    if output:
        Path(output).write_bytes(cert)
        print(f"OK: certificate written to {output}", file=sys.stderr)
    else:
        sys.stdout.buffer.write(cert)


def sign(args: Namespace) -> None:
    key_id = args.key_id or prompts.prompt("Key ID")
    principals = args.principals or prompts.prompt("Principals (comma-separated)")

    if args.force_command is None and not args.option and sys.stdin.isatty():
        forced_command = prompts.prompt_forced_command()
        extensions = prompts.prompt_extensions([] if forced_command else list(DEFAULT_EXTENSIONS))
        options = keygen.policy_options(forced_command, extensions)
    elif args.force_command is not None:
        options = ["clear", f"force-command={args.force_command}", *args.option]
    else:
        options = list(args.option)

    agents.unlock(USER_CA_KEY)
    pubkey = read_key_material(args.identity)
    emit(
        keygen.sign(USER_CA_KEY, key_id, principals, args.validity, pubkey, options=options),
        args.output,
    )


def renew(args: Namespace) -> None:
    cert = read_key_material(args.identity)
    info = keygen.describe(cert)
    if not info.key_id:
        raise ProvisionError("could not parse Key ID from certificate.")

    serial = args.serial if args.serial is not None else info.serial + 1
    validity = args.validity
    options = keygen.preserved_options(info)

    if sys.stdin.isatty() and args.serial is None:
        print_summary(info)
        validity = prompts.prompt_default("New validity", validity)

        if prompts.prompt_yes_no("Change any options?", default=False):
            forced_command = prompts.prompt_forced_command(current=info.force_command)
            extensions = prompts.prompt_extensions(info.extensions)
            options = keygen.policy_options(forced_command, extensions, info.other_critical)

    renewed = keygen.sign(
        USER_CA_KEY,
        info.key_id,
        ",".join(info.principals),
        validity,
        keygen.public_key_of(cert).encode() + b"\n",
        serial=serial,
        options=options,
    )
    emit(renewed, args.output)


def print_summary(info: keygen.CertInfo) -> None:
    print("\nCurrent certificate:", file=sys.stderr)
    print(f"  Key ID: {info.key_id}", file=sys.stderr)
    print(f"  Serial: {info.serial}", file=sys.stderr)
    print(f"  Principals: {', '.join(info.principals) or 'none'}", file=sys.stderr)
    print(f"  Forced command: {info.force_command or 'none'}", file=sys.stderr)
    print(f"  Extensions: {', '.join(info.extensions) or 'none'}", file=sys.stderr)
