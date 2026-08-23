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
# Config (env-overridable, same variable names as the old bash tool)
# --------------------------------------------------------------------------

HOST_CA_KEY = os.environ.get("MEL_HOST_CA_KEY", str(Path.home() / "ssh-ca-keys/ssh-host-ca"))
HOST_VALIDITY = os.environ.get("MEL_HOST_CERT_VALIDITY", "+30d")
HOST_CERT_PATH = os.environ.get("MEL_HOST_CERT_PATH", "/etc/ssh/ssh_host_ed25519_key-cert.pub")

USER_CA_KEY = os.environ.get("MEL_USER_CA_KEY", str(Path.home() / "ssh-ca-keys/ssh-user-ca"))
USER_VALIDITY = os.environ.get("MEL_USER_CERT_VALIDITY", "+52w")

# The agent that was already in SSH_AUTH_SOCK when we started -- i.e. the
# user's own agent (keychain/1Password/yubikey/etc). We always SSH to hosts
# through this one, via `-o IdentityAgent=...`, so host connections never
# touch the ephemeral CA agent and never prompt for anything the user
# hasn't already unlocked in their normal agent.
USER_SSH_AUTH_SOCK = os.environ.get("SSH_AUTH_SOCK", "")

CLEANUP_DIRS: list[str] = []
CA_AGENT_PID: int | None = None


# --------------------------------------------------------------------------
# Cleanup
# --------------------------------------------------------------------------


def cleanup() -> None:
    global CA_AGENT_PID
    if CA_AGENT_PID is not None:
        try:
            os.kill(CA_AGENT_PID, signal.SIGTERM)
        except ProcessLookupError:
            pass
        CA_AGENT_PID = None

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
        """Melinoe SSH Provisioning Tool

Usage:
  mel-ssh-provision host <command> [arguments]
  mel-ssh-provision user <command> [arguments]

Host commands:
  list
      List all hosts configured in the NixOS fleet.

  show <host>
      Show generated metadata and principals for a host.

  bootstrap <host>
      Generate host keys if necessary, sign them, and deploy the
      resulting certificate.

  rotate <host>
      Force regeneration of the target host keys, sign them, and
      redeploy the certificate.

  renew <host>
      Resign the existing host key and redeploy the certificate.

  renew-all
      Renew certificates for every configured host.

User commands:
  sign [options]
      Sign a user public key, producing a new user certificate.

  renew [options]
      Re-sign an existing user certificate, reusing its key ID,
      principals, critical options and extensions, with a bumped
      serial and a fresh validity window.

  Run 'mel-ssh-provision user sign -h' or 'user renew -h' for
  option details.""",
        file=sys.stderr,
    )
    sys.exit(1)


def setup_ca_agent(ca_key: str) -> None:
    """Unlock ca_key into a fresh, throwaway ssh-agent and point
    SSH_AUTH_SOCK at it for the rest of this process. Reused for every
    host/user operation within a single invocation, so you're only
    prompted for the CA passphrase once per run -- never for regular
    host logins, and never anywhere near the user's own agent."""
    global CA_AGENT_PID

    print("Unlocking CA key into ephemeral agent...", file=sys.stderr)

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
            # e.g. 'SSH_AGENT_PID=6360; export SSH_AGENT_PID;'
            value = line.split("=", 1)[1]
            value = value.split(";", 1)[0]
            pid = int(value)
            break
    if pid is None:
        die("could not determine ssh-agent PID")
    CA_AGENT_PID = pid

    os.environ["SSH_AUTH_SOCK"] = sock_path

    # ssh-add prompts for the passphrase over /dev/tty itself, so it works
    # fine even with stdout suppressed and stdin untouched.
    subprocess.run(["ssh-add", ca_key], check=True, stdout=subprocess.DEVNULL)


def read_key_material(identity: str | None) -> bytes:
    """Reads a public key (or certificate) from either a path argument,
    "-", or (if neither given) stdin -- prompting first if stdin is a
    terminal, so pasting works interactively."""
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


# --------------------------------------------------------------------------
# Host commands
# --------------------------------------------------------------------------


def require_host(host: str | None, hosts: dict) -> str:
    if not host:
        die("Missing required HOST argument.")
    if host not in hosts:
        die(f"Unknown host '{host}'.")
    return host


def process_host(host: str, force: bool, hosts: dict) -> None:
    require_host(host, hosts)
    target = hosts[host]["sshTarget"]
    principals = ",".join(hosts[host]["principals"])

    print(f"Processing {host} ({target})...")

    tmpdir = tempfile.mkdtemp()
    CLEANUP_DIRS.append(tmpdir)
    pubfile = os.path.join(tmpdir, "host.pub")
    certfile = os.path.join(tmpdir, "host-cert.pub")

    ssh_opts = []
    if USER_SSH_AUTH_SOCK:
        ssh_opts = ["-o", f"IdentityAgent={USER_SSH_AUTH_SOCK}"]

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
            ["ssh", *ssh_opts, f"root@{target}", "bash", "-s", "--", "true" if force else "false"],
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
            ["ssh", *ssh_opts, f"root@{target}", f"cat > '{HOST_CERT_PATH}' && systemctl reload sshd"],
            stdin=f,
            check=True,
        )

    print(f"OK: {host} updated successfully.")


def host_cmd(args: list[str], hosts: dict) -> None:
    if not args:
        usage()
    cmd, rest = args[0], args[1:]

    if cmd == "list":
        for h in sorted(hosts):
            print(h)

    elif cmd == "show":
        host = require_host(rest[0] if rest else None, hosts)
        print(json.dumps(hosts[host], indent=2))

    elif cmd in ("bootstrap", "rotate", "renew"):
        target_host = require_host(rest[0] if rest else None, hosts)
        setup_ca_agent(HOST_CA_KEY)
        process_host(target_host, force=(cmd == "rotate"), hosts=hosts)

    elif cmd == "renew-all":
        setup_ca_agent(HOST_CA_KEY)

        failures = []
        for h in sorted(hosts):
            try:
                process_host(h, force=False, hosts=hosts)
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
  -V, --validity SPEC   ssh-keygen -V validity spec (default: {USER_VALIDITY}).
  -O OPTION             Extra ssh-keygen -O option (critical option or
                         extension). May be given multiple times. If
                         omitted, ssh-keygen's default extension set is
                         used (permit-X11-forwarding, permit-agent-
                         forwarding, permit-port-forwarding, permit-pty,
                         permit-user-rc).
  -o, --output PATH     Where to write the resulting certificate.
                         Defaults to stdout.
  -h                    Show this help."""

USER_RENEW_USAGE = f"""Usage: mel-ssh-provision user renew [options]

Re-sign an existing user certificate: reuses its key ID, principals,
critical options and extensions verbatim, bumps the serial number,
and applies a fresh validity window.

Options:
  -i, --identity PATH   Path to the existing certificate. Use "-" or
                         omit to read from stdin (or paste
                         interactively).
  -V, --validity SPEC   ssh-keygen -V validity spec (default: {USER_VALIDITY}).
  -z, --serial N        Serial number to embed (default: old serial + 1).
  -o, --output PATH     Where to write the resulting certificate.
                         Defaults to stdout.
  -h                    Show this help."""


def _parse_flags(args: list[str], spec: dict[str, str], usage_text: str) -> dict:
    """Tiny getopt-alike. `spec` maps every recognised flag (long and
    short) to a dest name; a dest of "help" ends parsing immediately.
    "-O" is handled specially below (repeatable, appended to "O")."""
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
            "-i": "identity", "--identity": "identity",
            "-I": "key_id", "--key-id": "key_id",
            "-n": "principals", "--principals": "principals",
            "-V": "validity", "--validity": "validity",
            "-o": "output", "--output": "output",
        },
        USER_SIGN_USAGE,
    )

    key_id = opts.get("key_id") or prompt("Key ID")
    principals = opts.get("principals") or prompt("Principals (comma-separated)")
    validity = opts.get("validity", USER_VALIDITY)

    setup_ca_agent(USER_CA_KEY)

    pubkey = read_key_material(opts.get("identity"))
    do_sign_user(key_id, principals, validity, opts.get("output"), None, opts["O"], pubkey)


# ssh-keygen refuses to sign a certificate directly ("cannot be certified"),
# so to renew we have to pull the underlying base public key back out of
# the certificate's key blob and sign that. Supports ed25519, rsa and
# ecdsa certs (everything ssh-keygen itself can generate certs for, short
# of the FIDO/security-key variants).


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
    """Parses `ssh-keygen -L` output into key id / serial / principals /
    critical options / extensions."""
    key_id = ""
    old_serial = "0"
    principals: list[str] = []
    extra_opts: list[str] = []
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
            extra_opts.append(f"{name}={val}" if val else name)
        elif section == "extensions":
            extra_opts.append(line)

    return {
        "key_id": key_id,
        "old_serial": old_serial,
        "principals": principals,
        "extra_opts": extra_opts,
    }


def user_renew_cmd(args: list[str]) -> None:
    opts = _parse_flags(
        args,
        {
            "-i": "identity", "--identity": "identity",
            "-V": "validity", "--validity": "validity",
            "-z": "serial", "--serial": "serial",
            "-o": "output", "--output": "output",
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
        ["ssh-keygen", "-L", "-f", certfile], check=True, capture_output=True, text=True
    ).stdout
    fields = parse_cert_fields(keygen_l)

    if not fields["key_id"]:
        die("could not parse Key ID from certificate.")

    # Reconstruct extensions/critical options exactly rather than relying
    # on ssh-keygen's default extension set, which may not match what the
    # original cert had (e.g. a cert with no extensions at all, like a
    # force-command-only deploy cert).
    extra_opts = ["clear", *fields["extra_opts"]]

    serial = opts.get("serial") or str(int(fields["old_serial"]) + 1)
    principals_csv = ",".join(fields["principals"])
    pubkey_line = extract_pubkey_from_cert(cert_bytes)

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
        die("MEL_HOSTS_JSON_PATH is not set (this binary must be run via the Nix wrapper).")
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
        die(f"command failed ({' '.join(map(str, e.cmd))}): exit {e.returncode}")
    finally:
        cleanup()


if __name__ == "__main__":
    main()
