#!/usr/bin/env python3
"""Melinoe SSH Provisioning Tool.

Signs and rotates SSH host certificates for the fleet, and signs/renews
user certificates. See `usage()` below for the CLI surface.
"""

from __future__ import annotations

import base64
import json
import os
import shutil
import signal
import struct
import subprocess
import sys
import tempfile
from pathlib import Path

# --------------------------------------------------------------------------
# Config
# --------------------------------------------------------------------------

HOST_CA_KEY = os.environ.get(
    "MEL_HOST_CA_KEY", str(Path.home() / "ssh-ca-keys/ssh-host-ca")
)
HOST_VALIDITY = os.environ.get("MEL_HOST_CERT_VALIDITY", "+30d")
HOST_CERT_PATH = os.environ.get(
    "MEL_HOST_CERT_PATH", "/etc/ssh/ssh_host_ed25519_key-cert.pub"
)

USER_CA_KEY = os.environ.get(
    "MEL_USER_CA_KEY", str(Path.home() / "ssh-ca-keys/ssh-user-ca")
)
USER_VALIDITY = os.environ.get("MEL_USER_CERT_VALIDITY", "+52w")

# Separate (shorter) default for remote-build key certs specifically: these
# are renewed unattended, typically alongside host certs, rather than by a
# human renewing their own cert by hand - so they should default to a
# similar rotation cadence to host certs, not USER_VALIDITY's 1-year human
# default. Otherwise a cert signed today would outlive many renewal cycles
# before it mattered, which defeats renewing it on a schedule at all.
REMOTEBUILD_VALIDITY = os.environ.get("MEL_REMOTEBUILD_CERT_VALIDITY", HOST_VALIDITY)

USER_SSH_AUTH_SOCK = os.environ.get("SSH_AUTH_SOCK", "")

CLEANUP_DIRS: list[str] = []
CA_AGENT_PIDS: list[int] = []
CA_AGENT_SOCKS: dict[str, str] = {}

DEFAULT_EXTENSIONS = [
    "permit-X11-forwarding",
    "permit-agent-forwarding",
    "permit-port-forwarding",
    "permit-pty",
    "permit-user-rc",
]


# --------------------------------------------------------------------------
# Cleanup
# --------------------------------------------------------------------------


def cleanup() -> None:
    for pid in CA_AGENT_PIDS:
        try:
            os.kill(pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
    CA_AGENT_PIDS.clear()
    CA_AGENT_SOCKS.clear()

    for d in CLEANUP_DIRS:
        shutil.rmtree(d, ignore_errors=True)
    CLEANUP_DIRS.clear()


def _signal_exit(signum: int, _frame) -> None:
    cleanup()
    sys.exit(128 + signum)


for _sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
    signal.signal(_sig, _signal_exit)


# --------------------------------------------------------------------------
# Shared helpers
# --------------------------------------------------------------------------


def die(msg: str) -> None:
    print(f"ERROR: {msg}", file=sys.stderr)
    sys.exit(1)


def usage() -> None:
    print(
        f"""Melinoe SSH Provisioning Tool

Usage:
  mel-ssh-provision host <command> [arguments]
  mel-ssh-provision user <command> [arguments]

Host commands:
  list
      List all hosts configured in the NixOS fleet.

  show <host>
      Show generated metadata and principals for a host.

  bootstrap <host> [--remotebuild]
      Generate host keys if necessary, sign them, and deploy the
      resulting certificate.

  rotate <host> [--remotebuild]
      Force regeneration of the target host keys, sign them, and
      redeploy the certificate.

  renew <host> [--remotebuild]
      Resign the existing host key and redeploy the certificate.

  renew-all [--remotebuild]
      Renew certificates for every configured host.

      With --remotebuild, also runs the equivalent of
      renew-remotebuild on each host afterwards.

  renew-remotebuild <host>
      For each key path declared in this host's
      melinoe.node.remoteBuildOn.*.sshKey (in nix), checks whether
      "<key>-cert.pub" exists and is signed by our own user CA, and
      if so re-signs it in place, preserving its key ID, principals,
      and policy but applying a fresh validity window (default: same
      as host certs, {HOST_VALIDITY} - overridable via
      MEL_REMOTEBUILD_CERT_VALIDITY - since these are renewed
      unattended rather than by a human). Keys with no cert, or a
      cert signed by something else, are left untouched.

  renew-remotebuild-all
      Runs renew-remotebuild across every configured host.

User commands:
  sign [options]
      Sign a user public key, producing a new user certificate.

  renew [options]
      Re-sign an existing user certificate, reusing its key ID,
      principals, critical options and extensions, with a bumped
      serial and a fresh validity window. Interactive renewal can
      optionally change the certificate policy.

  Run 'mel-ssh-provision user sign -h' or 'user renew -h' for
  option details.""",
        file=sys.stderr,
    )
    sys.exit(1)


def setup_ca_agent(ca_key: str) -> None:
    """Unlock ca_key into an ephemeral ssh-agent and point SSH_AUTH_SOCK at
    it, so that subsequent `ssh-keygen -U` calls sign with it.

    Agents are cached per ca_key: calling this again for a key that is
    already unlocked just switches SSH_AUTH_SOCK back to its existing
    agent, instead of spawning a new one. This lets a single run hold
    both the host CA and user CA unlocked simultaneously and hop between
    them (e.g. renewing a host cert and then a remotebuild user cert on
    the same host).
    """
    if ca_key in CA_AGENT_SOCKS:
        os.environ["SSH_AUTH_SOCK"] = CA_AGENT_SOCKS[ca_key]
        return

    print(f"Unlocking CA key ({ca_key}) into ephemeral agent...", file=sys.stderr)

    ca_dir = tempfile.mkdtemp()
    CLEANUP_DIRS.append(ca_dir)
    sock_path = os.path.join(ca_dir, "agent.sock")

    out = subprocess.run(
        ["ssh-agent", "-a", sock_path],
        check=True,
        capture_output=True,
        text=True,
    ).stdout

    pid = None
    for line in out.splitlines():
        if line.startswith("SSH_AGENT_PID="):
            value = line.split("=", 1)[1]
            value = value.split(";", 1)[0]
            pid = int(value)
            break

    if pid is None:
        die("could not determine ssh-agent PID")

    CA_AGENT_PIDS.append(pid)
    os.environ["SSH_AUTH_SOCK"] = sock_path

    subprocess.run(
        ["ssh-add", ca_key],
        check=True,
        stdout=subprocess.DEVNULL,
    )

    CA_AGENT_SOCKS[ca_key] = sock_path


def read_key_material(identity: str | None) -> bytes:
    """Read a public key or certificate from a path, -, or stdin."""
    if identity and identity != "-":
        p = Path(identity)
        if not p.is_file():
            die(f"file not found: {identity}")
        return p.read_bytes()

    if sys.stdin.isatty():
        print("Paste the key, then press Ctrl-D:", file=sys.stderr)

    return sys.stdin.buffer.read()


def prompt(msg: str) -> str:
    if not sys.stdin.isatty():
        die(f"{msg} is required (not running interactively to prompt for it).")

    print(f"{msg}: ", end="", file=sys.stderr, flush=True)
    return input()


def prompt_default(msg: str, default: str) -> str:
    if not sys.stdin.isatty():
        return default

    print(f"{msg} [{default}]: ", end="", file=sys.stderr, flush=True)
    value = input()
    return value if value else default


def prompt_yes_no(msg: str, default: bool = False) -> bool:
    suffix = "Y/n" if default else "y/N"

    while True:
        print(f"{msg} [{suffix}]: ", end="", file=sys.stderr, flush=True)
        value = input().strip().lower()

        if not value:
            return default
        if value in ("y", "yes"):
            return True
        if value in ("n", "no"):
            return False

        print("Please answer y or n.", file=sys.stderr)


# --------------------------------------------------------------------------
# Extension selection
# --------------------------------------------------------------------------


def format_extensions(extensions: list[str]) -> str:
    if not extensions:
        return "none"
    return ", ".join(extensions)


def parse_extension_selection(
    value: str,
    extensions: list[str],
) -> list[str]:
    """Parse n/y, numbers, comma-separated numbers, and numeric ranges."""
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
            bits = part.split("-", 1)
            if len(bits) != 2 or not bits[0].isdigit() or not bits[1].isdigit():
                raise ValueError(f"invalid range: {part}")

            start = int(bits[0])
            end = int(bits[1])

            if start > end:
                raise ValueError(f"invalid range: {part}")

            selected.update(range(start, end + 1))
            continue

        if not part.isdigit():
            raise ValueError(f"invalid selection: {part}")

        selected.add(int(part))

    for number in selected:
        if number < 1 or number > len(extensions):
            raise ValueError(f"selection out of range: {number}")

    return [extensions[i - 1] for i in sorted(selected)]


def prompt_extensions(default: list[str]) -> list[str]:
    print("\nExtensions:", file=sys.stderr)

    for i, extension in enumerate(DEFAULT_EXTENSIONS, 1):
        print(f"  {i}. {extension}", file=sys.stderr)

    default_text = (
        "none" if not default else ",".join(
            str(DEFAULT_EXTENSIONS.index(ext) + 1)
            for ext in default
            if ext in DEFAULT_EXTENSIONS
        )
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


# --------------------------------------------------------------------------
# Certificate policy
# --------------------------------------------------------------------------


def prompt_forced_command(
    current: str | None = None,
    *,
    new_certificate: bool = False,
) -> str | None:
    if current is None:
        label = "Forced command [N/<command>]"
        default = None
    else:
        label = f"Forced command [current: {current}, N/<command>]"
        default = current

    print(f"{label}: ", end="", file=sys.stderr, flush=True)
    value = input().strip()

    if not value:
        return default

    if value.lower() in ("n", "none"):
        return None

    return value


def interactive_sign_policy() -> tuple[str | None, list[str]]:
    forced_command = prompt_forced_command(new_certificate=True)

    if forced_command:
        default_extensions: list[str] = []
    else:
        default_extensions = list(DEFAULT_EXTENSIONS)

    extensions = prompt_extensions(default_extensions)

    return forced_command, extensions


def interactive_renew_policy(
    current_forced_command: str | None,
    current_extensions: list[str],
) -> tuple[str | None, list[str]]:
    forced_command = prompt_forced_command(current=current_forced_command)

    # For an existing certificate, the current extensions are always
    # the default. We deliberately do not infer a new default from the
    # forced-command value here.
    extensions = prompt_extensions(current_extensions)

    return forced_command, extensions


def extension_opts(extensions: list[str]) -> list[str]:
    return [f"{extension}" for extension in extensions]


def build_policy_opts(
    forced_command: str | None,
    extensions: list[str],
) -> list[str]:
    """Turn policy into ssh-keygen -O arguments.

    'clear' is necessary whenever we are deliberately specifying an
    extension set, because ssh-keygen otherwise supplies its default
    extension set.

    This is particularly important for forced-command certificates:
    forced command + no extensions must really mean no extensions.
    """
    opts: list[str] = ["clear"]

    if forced_command:
        opts.append(f"force-command={forced_command}")

    opts.extend(extension_opts(extensions))
    return opts


# --------------------------------------------------------------------------
# Host commands
# --------------------------------------------------------------------------


def require_host(host: str | None, hosts: dict) -> str:
    if not host:
        die("Missing required HOST argument.")
    if host not in hosts:
        die(f"Unknown host '{host}'.")
    return host


def ssh_opts() -> list[str]:
    """SSH options for connecting *to* fleet hosts as root.

    Deliberately independent of whatever CA agent is currently unlocked
    (see setup_ca_agent) - this always points at the invoking user's own
    agent, so it keeps working no matter which CA key is active.
    """
    if USER_SSH_AUTH_SOCK:
        return ["-o", f"IdentityAgent={USER_SSH_AUTH_SOCK}"]
    return []


def process_host(host: str, force: bool, hosts: dict) -> None:
    require_host(host, hosts)

    target = hosts[host]["sshTarget"]
    principals = ",".join(hosts[host]["principals"])

    print(f"Processing {host} ({target})...")

    tmpdir = tempfile.mkdtemp()
    CLEANUP_DIRS.append(tmpdir)
    pubfile = os.path.join(tmpdir, "host.pub")
    certfile = os.path.join(tmpdir, "host-cert.pub")

    opts = ssh_opts()

    remote_script = """set -euo pipefail

key=/etc/ssh/ssh_host_ed25519_key
pub=/etc/ssh/ssh_host_ed25519_key.pub

if [ "$1" = "true" ] || [ ! -s "$pub" ]; then
  rm -f "$key" "$pub"
  ssh-keygen -t ed25519 -N "" -f "$key" -q
fi

cat "$pub"
"""

    with open(pubfile, "wb") as f:
        subprocess.run(
            [
                "ssh",
                *opts,
                f"root@{target}",
                "bash",
                "-s",
                "--",
                "true" if force else "false",
            ],
            input=remote_script.encode(),
            stdout=f,
            check=True,
        )

    subprocess.run(
        [
            "ssh-keygen",
            "-q",
            "-U",
            "-s",
            f"{HOST_CA_KEY}.pub",
            "-I",
            host,
            "-h",
            "-n",
            principals,
            "-V",
            HOST_VALIDITY,
            pubfile,
        ],
        check=True,
    )

    if not os.path.isfile(certfile):
        die(f"certificate not generated: {certfile}")

    with open(certfile, "rb") as f:
        subprocess.run(
            [
                "ssh",
                *opts,
                f"root@{target}",
                f"cat > '{HOST_CERT_PATH}' && systemctl reload sshd",
            ],
            stdin=f,
            check=True,
        )

    print(f"OK: {host} updated successfully.")


# --------------------------------------------------------------------------
# Remotebuild user-cert renewal
# --------------------------------------------------------------------------


def key_fingerprint(pubkey_path: str) -> str:
    """SHA256 fingerprint (as printed by `ssh-keygen -l`) of a public key file."""
    out = subprocess.run(
        ["ssh-keygen", "-l", "-f", pubkey_path],
        check=True,
        capture_output=True,
        text=True,
    ).stdout
    # e.g. "256 SHA256:AbCdEf... comment (ED25519)"
    return out.split()[1]


def cert_signing_ca_fingerprint(ssh_keygen_dash_l_output: str) -> str | None:
    """Pull the "SHA256:..." fingerprint out of a `ssh-keygen -L` Signing CA line."""
    for raw in ssh_keygen_dash_l_output.splitlines():
        line = raw.strip()
        if line.startswith("Signing CA:"):
            for token in line.split():
                if token.startswith("SHA256:"):
                    return token
    return None


def preserved_extra_opts(fields: dict) -> list[str]:
    """Rebuild the -O options for a renewal that keeps the existing policy
    (critical options and extensions) exactly as it was."""
    extra_opts = ["clear", *fields["other_critical_opts"]]

    if fields["force_command"]:
        extra_opts.insert(1, f"force-command={fields['force_command']}")

    extra_opts.extend(fields["extensions"])
    return extra_opts


def split_cert_fields(fields: dict) -> dict:
    """Split parse_cert_fields()'s flat extra_opts into extensions vs. other
    critical options, the way user_renew_cmd does. Mutates and returns fields."""
    fields["extensions"] = [
        o for o in fields["extra_opts"] if o in DEFAULT_EXTENSIONS
    ]
    fields["other_critical_opts"] = [
        o for o in fields["extra_opts"] if o not in DEFAULT_EXTENSIONS
    ]
    return fields


def sync_remotebuild_certs(host: str, target: str, key_paths: list[str]) -> list[str]:
    """For each of `host`'s declared remote-build key paths (from
    melinoe.node.remoteBuildOn.*.sshKey in nix), check whether
    "<key_path>-cert.pub" exists on `target` and is signed by our own
    user CA, and if so re-sign it in place, preserving its existing
    policy.

    Returns one human-readable status line per key path. Assumes the
    user CA is already unlocked via setup_ca_agent(USER_CA_KEY).
    """
    if not key_paths:
        return ["no remote-build keys declared for this host"]

    statuses = []

    for key_path in key_paths:
        cert_path = f"{key_path}-cert.pub"
        statuses.append(sync_one_cert(target, cert_path))

    return statuses


def sync_one_cert(target: str, cert_path: str) -> str:
    tmpdir = tempfile.mkdtemp()
    CLEANUP_DIRS.append(tmpdir)
    certfile = os.path.join(tmpdir, "cert.pub")

    remote_check = f"""set -euo pipefail
if [ -s '{cert_path}' ]; then
  cat '{cert_path}'
else
  exit 3
fi
"""

    result = subprocess.run(
        ["ssh", *ssh_opts(), f"root@{target}", "bash", "-s"],
        input=remote_check.encode(),
        capture_output=True,
    )

    if result.returncode == 3:
        return f"{cert_path}: not present, skipped"

    if result.returncode != 0:
        raise subprocess.CalledProcessError(
            result.returncode,
            f"check {cert_path} on {target}",
            output=result.stdout,
            stderr=result.stderr,
        )

    cert_bytes = result.stdout
    Path(certfile).write_bytes(cert_bytes)

    keygen_l = subprocess.run(
        ["ssh-keygen", "-L", "-f", certfile],
        check=True,
        capture_output=True,
        text=True,
    ).stdout

    signing_fp = cert_signing_ca_fingerprint(keygen_l)
    if signing_fp != key_fingerprint(f"{USER_CA_KEY}.pub"):
        return f"{cert_path}: not signed by our user CA, skipped"

    fields = split_cert_fields(parse_cert_fields(keygen_l))

    if not fields["key_id"]:
        return f"{cert_path}: could not parse certificate, skipped"

    serial = str(int(fields["old_serial"]) + 1)
    principals_csv = ",".join(fields["principals"])
    pubkey_line = extract_pubkey_from_cert(cert_bytes)
    extra_opts = preserved_extra_opts(fields)

    new_certfile = os.path.join(tmpdir, "cert.pub.new")

    setup_ca_agent(USER_CA_KEY)
    do_sign_user(
        fields["key_id"],
        principals_csv,
        REMOTEBUILD_VALIDITY,
        new_certfile,
        serial,
        extra_opts,
        pubkey_line.encode() + b"\n",
    )

    with open(new_certfile, "rb") as f:
        subprocess.run(
            ["ssh", *ssh_opts(), f"root@{target}", f"cat > '{cert_path}'"],
            stdin=f,
            check=True,
        )

    return f"{cert_path}: renewed (serial {serial})"


def print_remotebuild_statuses(host: str, hosts: dict) -> None:
    key_paths = hosts[host].get("remoteBuildKeys", [])
    for status in sync_remotebuild_certs(host, hosts[host]["sshTarget"], key_paths):
        print(f"{host}: {status}")


def host_cmd(args: list[str], hosts: dict) -> None:
    if not args:
        usage()

    cmd, rest = args[0], args[1:]

    flags = [a for a in rest if a.startswith("-")]
    positional = [a for a in rest if not a.startswith("-")]
    with_remotebuild = "--remotebuild" in flags

    if cmd == "list":
        for h in sorted(hosts):
            print(h)

    elif cmd == "show":
        host = require_host(positional[0] if positional else None, hosts)
        print(json.dumps(hosts[host], indent=2))

    elif cmd in ("bootstrap", "rotate", "renew"):
        target_host = require_host(positional[0] if positional else None, hosts)
        setup_ca_agent(HOST_CA_KEY)
        process_host(target_host, force=(cmd == "rotate"), hosts=hosts)

        if with_remotebuild:
            setup_ca_agent(USER_CA_KEY)
            print_remotebuild_statuses(target_host, hosts)

    elif cmd == "renew-all":
        setup_ca_agent(HOST_CA_KEY)
        if with_remotebuild:
            setup_ca_agent(USER_CA_KEY)

        failures = []

        for h in sorted(hosts):
            try:
                setup_ca_agent(HOST_CA_KEY)
                process_host(h, force=False, hosts=hosts)

                if with_remotebuild:
                    setup_ca_agent(USER_CA_KEY)
                    print_remotebuild_statuses(h, hosts)
            except subprocess.CalledProcessError:
                print(f"FAILED: {h}", file=sys.stderr)
                failures.append(h)

        if failures:
            print(
                f"\nCompleted with {len(failures)} failure(s): {' '.join(failures)}",
                file=sys.stderr,
            )
            sys.exit(1)

        print("All hosts renewed successfully.")

    elif cmd == "renew-remotebuild":
        target_host = require_host(positional[0] if positional else None, hosts)
        setup_ca_agent(USER_CA_KEY)
        print_remotebuild_statuses(target_host, hosts)

    elif cmd == "renew-remotebuild-all":
        setup_ca_agent(USER_CA_KEY)

        failures = []

        for h in sorted(hosts):
            try:
                print_remotebuild_statuses(h, hosts)
            except subprocess.CalledProcessError:
                print(f"FAILED: {h}", file=sys.stderr)
                failures.append(h)

        if failures:
            print(
                f"\nCompleted with {len(failures)} failure(s): {' '.join(failures)}",
                file=sys.stderr,
            )
            sys.exit(1)

        print("Remotebuild key certs synced for all hosts.")

    else:
        usage()


# --------------------------------------------------------------------------
# User commands
# --------------------------------------------------------------------------


USER_SIGN_USAGE = f"""Usage: mel-ssh-provision user sign [options]

Sign a user public key, producing a new user certificate.

Options:
  -i, --identity PATH   Path to the public key to sign. Use "-" or omit
                        to read from stdin (or paste interactively).
  -I, --key-id ID       Key ID to embed in the certificate. Prompted for
                        if omitted and running interactively.
  -n, --principals LIST Comma-separated list of principals. Prompted for
                        if omitted and running interactively.
  --force-command CMD   Restrict the certificate to CMD. When supplied,
                        default extensions are cleared; explicit -O
                        options are applied afterwards.
  -V, --validity SPEC   ssh-keygen -V validity spec (default: {USER_VALIDITY}).
  -O OPTION             Extra ssh-keygen -O option (critical option or
                        extension). May be given multiple times.
  -o, --output PATH     Where to write the resulting certificate.
                        Defaults to stdout.
  -h                    Show this help.

Interactive signing asks for the forced command first, followed by
extension selection. A forced command defaults to no extensions;
otherwise all standard OpenSSH extensions are offered by default.
"""


USER_RENEW_USAGE = f"""Usage: mel-ssh-provision user renew [options]

Re-sign an existing user certificate: reuses its key ID, principals,
critical options and extensions, bumps the serial number, and applies
a fresh validity window.

When run interactively, the existing certificate policy is displayed
and can optionally be changed before renewal.

Options:
  -i, --identity PATH   Path to the existing certificate. Use "-" or omit
                        to read from stdin (or paste interactively).
  -V, --validity SPEC   ssh-keygen -V validity spec (default: {USER_VALIDITY}).
  -z, --serial N        Serial number to embed (default: old serial + 1).
  -o, --output PATH     Where to write the resulting certificate.
                        Defaults to stdout.
  -h                    Show this help.
"""


def _parse_flags(
    args: list[str],
    spec: dict[str, str],
    usage_text: str,
) -> dict:
    """Tiny getopt-alike.

    `spec` maps recognised flags to destination names.
    `-O` is repeatable and appended to "O".
    """
    out: dict = {"O": []}
    i = 0

    while i < len(args):
        a = args[i]

        if a in ("-h", "--help"):
            print(usage_text)
            sys.exit(0)

        if a == "-O":
            if i + 1 >= len(args):
                die(f"missing value for {a}")
            out["O"].append(args[i + 1])
            i += 2
            continue

        if a in spec:
            if i + 1 >= len(args):
                die(f"missing value for {a}")
            out[spec[a]] = args[i + 1]
            i += 2
            continue

        print(f"ERROR: unknown argument: {a}", file=sys.stderr)
        print(usage_text, file=sys.stderr)
        sys.exit(1)

    return out


def do_sign_user(
    key_id: str,
    principals: str,
    validity: str,
    out_path: str | None,
    serial: str | None,
    extra_opts: list[str],
    pubkey: bytes,
) -> None:
    tmpdir = tempfile.mkdtemp()
    CLEANUP_DIRS.append(tmpdir)

    pubfile = os.path.join(tmpdir, "user.pub")
    certfile = os.path.join(tmpdir, "user-cert.pub")

    Path(pubfile).write_bytes(pubkey)

    cmd = [
        "ssh-keygen",
        "-q",
        "-U",
        "-s",
        f"{USER_CA_KEY}.pub",
        "-I",
        key_id,
        "-n",
        principals,
        "-V",
        validity,
    ]

    if serial:
        cmd += ["-z", str(serial)]

    for o in extra_opts:
        cmd += ["-O", o]

    cmd.append(pubfile)

    subprocess.run(cmd, check=True)

    if not os.path.isfile(certfile):
        die(f"certificate not generated: {certfile}")

    if out_path:
        shutil.copy(certfile, out_path)
        print(f"OK: certificate written to {out_path}", file=sys.stderr)
    else:
        sys.stdout.buffer.write(Path(certfile).read_bytes())


def user_sign_cmd(args: list[str]) -> None:
    opts = _parse_flags(
        args,
        {
            "-i": "identity",
            "--identity": "identity",
            "-I": "key_id",
            "--key-id": "key_id",
            "-n": "principals",
            "--principals": "principals",
            "--force-command": "force_command",
            "-V": "validity",
            "--validity": "validity",
            "-o": "output",
            "--output": "output",
        },
        USER_SIGN_USAGE,
    )

    key_id = opts.get("key_id") or prompt("Key ID")
    principals = opts.get("principals") or prompt("Principals (comma-separated)")
    validity = opts.get("validity", USER_VALIDITY)

    # Interactive policy construction.
    if (
        "force_command" not in opts
        and not opts["O"]
        and sys.stdin.isatty()
    ):
        force_command, extensions = interactive_sign_policy()
        extra_opts = build_policy_opts(force_command, extensions)

    else:
        force_command = opts.get("force_command")

        if force_command is not None:
            # A forced command is deliberately restrictive unless the
            # caller explicitly supplies extension options.
            extra_opts = ["clear", f"force-command={force_command}"]
            extra_opts.extend(opts["O"])

        elif opts["O"]:
            # Preserve raw ssh-keygen semantics for expert/scripted use.
            extra_opts = list(opts["O"])

        else:
            # No forced command and no explicit options: let ssh-keygen
            # provide its normal extension defaults.
            extra_opts = []

    setup_ca_agent(USER_CA_KEY)

    pubkey = read_key_material(opts.get("identity"))

    do_sign_user(
        key_id,
        principals,
        validity,
        opts.get("output"),
        None,
        extra_opts,
        pubkey,
    )


# --------------------------------------------------------------------------
# OpenSSH certificate parsing
# --------------------------------------------------------------------------


def _read_wire_string(buf: bytes, off: int) -> tuple[bytes, int]:
    (length,) = struct.unpack_from(">I", buf, off)
    off += 4
    return buf[off : off + length], off + length


def _wire_string(b: bytes) -> bytes:
    return struct.pack(">I", len(b)) + b


def extract_pubkey_from_cert(cert_bytes: bytes) -> str:
    line = cert_bytes.decode().splitlines()[0].strip()
    parts = line.split(None, 2)

    if len(parts) < 2:
        die("could not parse certificate public key line")

    wire_type_str, b64 = parts[0], parts[1]

    blob = base64.b64decode(b64)
    off = 0

    wire_type, off = _read_wire_string(blob, off)
    wire_type = wire_type.decode()

    if not wire_type.endswith("-cert-v01@openssh.com"):
        die(f"not a certificate public key (got type {wire_type})")

    _, off = _read_wire_string(blob, off)  # nonce

    if wire_type.startswith("ssh-ed25519-cert"):
        base_type = "ssh-ed25519"
        pk, off = _read_wire_string(blob, off)
        fields = [pk]

    elif wire_type.startswith("ssh-rsa-cert"):
        base_type = "ssh-rsa"
        e, off = _read_wire_string(blob, off)
        n, off = _read_wire_string(blob, off)
        fields = [e, n]

    elif wire_type.startswith("ecdsa-sha2-") and "-cert" in wire_type:
        curve_id = wire_type.split("ecdsa-sha2-")[1].split("-cert")[0]
        base_type = f"ecdsa-sha2-{curve_id}"
        curve, off = _read_wire_string(blob, off)
        q, off = _read_wire_string(blob, off)
        fields = [curve, q]

    else:
        die(f"unsupported certificate key type: {wire_type}")

    out = _wire_string(base_type.encode())

    for f in fields:
        out += _wire_string(f)

    return f"{base_type} {base64.b64encode(out).decode()}"


def parse_cert_fields(ssh_keygen_dash_l_output: str) -> dict:
    """Parse ssh-keygen -L output."""
    key_id = ""
    old_serial = "0"
    principals: list[str] = []
    extra_opts: list[str] = []
    force_command: str | None = None
    section: str | None = None

    for raw in ssh_keygen_dash_l_output.splitlines():
        line = raw.strip()

        if "Key ID:" in line:
            key_id = line.split('"', 2)[1]
            section = None
            continue

        if line.startswith("Serial:"):
            old_serial = line.split(":", 1)[1].strip()
            section = None
            continue

        if line.startswith("Principals:"):
            section = "principals"
            continue

        if line.startswith("Critical Options:"):
            section = None if "(none)" in line else "critical"
            continue

        if line.startswith("Extensions:"):
            section = None if "(none)" in line else "extensions"
            continue

        if line.startswith("Valid:"):
            section = None
            continue

        if not line:
            continue

        if section == "principals":
            principals.append(line)

        elif section == "critical":
            name, _, val = line.partition(" ")

            if name == "force-command":
                force_command = val
            else:
                extra_opts.append(f"{name}={val}" if val else name)

        elif section == "extensions":
            extra_opts.append(line)

    return {
        "key_id": key_id,
        "old_serial": old_serial,
        "principals": principals,
        "extra_opts": extra_opts,
        "force_command": force_command,
    }


# --------------------------------------------------------------------------
# User renewal
# --------------------------------------------------------------------------


def print_cert_summary(fields: dict) -> None:
    print("\nCurrent certificate:", file=sys.stderr)
    print(f"  Key ID: {fields['key_id']}", file=sys.stderr)
    print(f"  Serial: {fields['old_serial']}", file=sys.stderr)
    print(
        f"  Principals: {', '.join(fields['principals']) or 'none'}",
        file=sys.stderr,
    )
    print(
        f"  Forced command: {fields['force_command'] or 'none'}",
        file=sys.stderr,
    )
    print(
        f"  Extensions: {format_extensions(fields['extensions'])}",
        file=sys.stderr,
    )


def user_renew_cmd(args: list[str]) -> None:
    opts = _parse_flags(
        args,
        {
            "-i": "identity",
            "--identity": "identity",
            "-V": "validity",
            "--validity": "validity",
            "-z": "serial",
            "--serial": "serial",
            "-o": "output",
            "--output": "output",
        },
        USER_RENEW_USAGE,
    )

    validity = opts.get("validity", USER_VALIDITY)

    tmpdir = tempfile.mkdtemp()
    CLEANUP_DIRS.append(tmpdir)

    certfile = os.path.join(tmpdir, "existing-cert.pub")

    cert_bytes = read_key_material(opts.get("identity"))
    Path(certfile).write_bytes(cert_bytes)

    keygen_l = subprocess.run(
        ["ssh-keygen", "-L", "-f", certfile],
        check=True,
        capture_output=True,
        text=True,
    ).stdout

    fields = parse_cert_fields(keygen_l)

    if not fields["key_id"]:
        die("could not parse Key ID from certificate.")

    # Split the parsed policy into its user-facing pieces.
    fields = split_cert_fields(fields)

    serial = opts.get("serial") or str(int(fields["old_serial"]) + 1)
    principals_csv = ",".join(fields["principals"])
    pubkey_line = extract_pubkey_from_cert(cert_bytes)

    if sys.stdin.isatty() and opts.get("serial") is None:
        print_cert_summary(fields)

        validity = prompt_default("New validity", validity)

        if prompt_yes_no("Change any options?", default=False):
            (
                forced_command,
                extensions,
            ) = interactive_renew_policy(
                fields["force_command"],
                fields["extensions"],
            )

            extra_opts = ["clear"]

            if forced_command:
                extra_opts.append(f"force-command={forced_command}")

            extra_opts.extend(fields["other_critical_opts"])
            extra_opts.extend(extension_opts(extensions))

        else:
            # Preserve the existing policy exactly.
            extra_opts = preserved_extra_opts(fields)

    else:
        # Non-interactive renewal preserves the existing certificate
        # policy exactly, as it did before.
        extra_opts = preserved_extra_opts(fields)

    setup_ca_agent(USER_CA_KEY)

    do_sign_user(
        fields["key_id"],
        principals_csv,
        validity,
        opts.get("output"),
        serial,
        extra_opts,
        pubkey_line.encode() + b"\n",
    )


def user_cmd(args: list[str]) -> None:
    if not args:
        usage()

    cmd, rest = args[0], args[1:]

    if cmd == "sign":
        user_sign_cmd(rest)
    elif cmd == "renew":
        user_renew_cmd(rest)
    else:
        usage()


# --------------------------------------------------------------------------
# Dispatch
# --------------------------------------------------------------------------


def main() -> None:
    hosts_json_path = os.environ.get("MEL_HOSTS_JSON_PATH")

    if not hosts_json_path:
        die(
            "MEL_HOSTS_JSON_PATH is not set "
            "(this binary must be run via the Nix wrapper)."
        )

    hosts = json.loads(Path(hosts_json_path).read_text())

    args = sys.argv[1:]

    if not args:
        usage()

    top, rest = args[0], args[1:]

    try:
        if top == "host":
            host_cmd(rest, hosts)
        elif top == "user":
            user_cmd(rest)
        else:
            usage()
    except subprocess.CalledProcessError as e:
        die(
            f"command failed ({' '.join(map(str, e.cmd))}): "
            f"exit {e.returncode}"
        )
    finally:
        cleanup()


if __name__ == "__main__":
    main()
