import argparse
import signal
import subprocess
import sys

from . import hosts, users
from .agents import agents
from .errors import ProvisionError
from .settings import USER_VALIDITY


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="mel-ssh-provision",
        description="Melinoe SSH Provisioning Tool",
        allow_abbrev=False,
    )
    groups = parser.add_subparsers(dest="group", required=True)

    host = groups.add_parser("host", help="host certificates", allow_abbrev=False)
    host_cmds = host.add_subparsers(dest="command", required=True)

    host_cmds.add_parser("list", help="list the hosts configured in the NixOS fleet")

    show = host_cmds.add_parser("show", help="show generated metadata and principals for a host")
    show.add_argument("host")

    remotebuild_help = (
        "also handle each declared remote-build key: generate it if missing and sign it with "
        "the user CA, using the declared sshUser(s) as principals, key ID remotebuild-<host>, "
        "and no extensions or critical options"
    )
    for name, help_text in (
        (
            "bootstrap",
            "generate host keys if necessary, sign them and deploy the certificate",
        ),
        ("rotate", "regenerate the host keys, sign them and redeploy the certificate"),
        ("renew", "resign the existing host key and redeploy the certificate"),
    ):
        cmd = host_cmds.add_parser(name, help=help_text)
        cmd.add_argument("host")
        cmd.add_argument("--remotebuild", action="store_true", help=remotebuild_help)

    renew_all = host_cmds.add_parser("renew-all", help="renew certificates for every host")
    renew_all.add_argument("--remotebuild", action="store_true", help=remotebuild_help)

    renew_rb = host_cmds.add_parser(
        "renew-remotebuild",
        help="re-sign declared remote-build key certs that our user CA signed, keeping their policy",
    )
    renew_rb.add_argument("host")
    host_cmds.add_parser("renew-remotebuild-all", help="renew-remotebuild for every host")

    user = groups.add_parser("user", help="user certificates", allow_abbrev=False)
    user_cmds = user.add_subparsers(dest="command", required=True)

    sign = user_cmds.add_parser(
        "sign",
        help="sign a user public key",
        allow_abbrev=False,
        description=(
            "Interactive signing asks for the forced command first, then extension selection. "
            "A forced command defaults to no extensions; otherwise all standard OpenSSH "
            "extensions are offered by default."
        ),
    )
    sign.add_argument("-i", "--identity", help='public key to sign; "-" or omit to read stdin')
    sign.add_argument("-I", "--key-id")
    sign.add_argument("-n", "--principals", help="comma-separated")
    sign.add_argument(
        "--force-command",
        help="restrict the certificate to this command, clearing the default extensions",
    )
    sign.add_argument("-V", "--validity", default=USER_VALIDITY, help="ssh-keygen -V spec")
    sign.add_argument(
        "-O",
        "--option",
        action="append",
        default=[],
        help="extra ssh-keygen -O option; repeatable",
    )
    sign.add_argument("-o", "--output", help="defaults to stdout")

    renew = user_cmds.add_parser(
        "renew",
        help="re-sign an existing user certificate",
        allow_abbrev=False,
        description=(
            "Reuses the key ID, principals, critical options and extensions with a bumped "
            "serial and fresh validity. Interactively the policy can be changed first."
        ),
    )
    renew.add_argument("-i", "--identity", help='certificate to renew; "-" or omit to read stdin')
    renew.add_argument("-V", "--validity", default=USER_VALIDITY, help="ssh-keygen -V spec")
    renew.add_argument("-z", "--serial", type=int, help="defaults to the old serial + 1")
    renew.add_argument("-o", "--output", help="defaults to stdout")

    return parser


def dispatch(args: argparse.Namespace) -> None:
    if args.group == "user":
        (users.sign if args.command == "sign" else users.renew)(args)
        return

    fleet = hosts.load_hosts()
    match args.command:
        case "list":
            hosts.list_hosts(fleet)
        case "show":
            hosts.show(fleet, args.host)
        case "bootstrap" | "rotate" | "renew":
            hosts.provision(fleet, args.command, args.host, args.remotebuild)
        case "renew-all":
            hosts.renew_all(fleet, args.remotebuild)
        case "renew-remotebuild":
            hosts.renew_remotebuild(fleet, args.host)
        case "renew-remotebuild-all":
            hosts.renew_remotebuild_all(fleet)


def main() -> int:
    def terminate(signum: int, _frame) -> None:
        agents.close()
        sys.exit(128 + signum)

    for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
        signal.signal(sig, terminate)

    args = build_parser().parse_args()

    try:
        dispatch(args)
    except ProvisionError as e:
        print(f"ERROR: {e}", file=sys.stderr)
        return 1
    except subprocess.CalledProcessError as e:
        print(
            f"ERROR: command failed ({' '.join(map(str, e.cmd))}): exit {e.returncode}",
            file=sys.stderr,
        )
        return 1
    finally:
        agents.close()
    return 0
