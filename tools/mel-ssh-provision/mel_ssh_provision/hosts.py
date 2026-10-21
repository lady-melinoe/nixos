import json
import os
import subprocess
import sys
from pathlib import Path

from . import keygen, remote
from .agents import agents
from .errors import ProvisionError
from .settings import (
    HOST_CA_KEY,
    HOST_VALIDITY,
    REMOTEBUILD_VALIDITY,
    USER_CA_KEY,
)


def load_hosts() -> dict:
    path = os.environ.get("MEL_HOSTS_JSON_PATH")
    if not path:
        raise ProvisionError(
            "MEL_HOSTS_JSON_PATH is not set (this binary must be run via the Nix wrapper)."
        )
    return json.loads(Path(path).read_text())


def require_host(hosts: dict, host: str) -> dict:
    if host not in hosts:
        raise ProvisionError(f"Unknown host '{host}'.")
    return hosts[host]


def process_host(name: str, meta: dict, force: bool) -> None:
    target = meta["sshTarget"]
    print(f"Processing {name} ({target})...")

    pubkey = remote.run_script(target, remote.HOST_KEY_SCRIPT, "true" if force else "false").stdout

    cert = keygen.sign(
        HOST_CA_KEY,
        name,
        ",".join(meta["principals"]),
        HOST_VALIDITY,
        pubkey,
        host=True,
    )

    cert_path = os.environ.get("MEL_HOST_CERT_PATH", meta["hostCertificate"])
    remote.write_file(target, cert_path, cert, then="systemctl reload sshd")

    print(f"OK: {name} updated successfully.")


def renew_remotebuild_cert(target: str, cert_path: str) -> str:
    result = remote.run_script(target, remote.CERT_READ_SCRIPT, cert_path, check=False)
    if result.returncode == 3:
        return f"{cert_path}: not present, skipped"
    result.check_returncode()

    cert = result.stdout
    info = keygen.describe(cert)

    if info.signing_ca != keygen.fingerprint(f"{USER_CA_KEY}.pub"):
        return f"{cert_path}: not signed by our user CA, skipped"
    if not info.key_id:
        return f"{cert_path}: could not parse certificate, skipped"

    serial = info.serial + 1
    renewed = keygen.sign(
        USER_CA_KEY,
        info.key_id,
        ",".join(info.principals),
        REMOTEBUILD_VALIDITY,
        keygen.public_key_of(cert).encode() + b"\n",
        serial=serial,
        options=keygen.preserved_options(info),
    )
    remote.write_file(target, cert_path, renewed)

    return f"{cert_path}: renewed (serial {serial})"


def provision_remotebuild_key(
    name: str, target: str, key_path: str, users: list[str], force: bool
) -> str:
    pubkey = remote.run_script(
        target, remote.REMOTEBUILD_KEY_SCRIPT, key_path, "true" if force else "false"
    ).stdout

    cert = keygen.sign(
        USER_CA_KEY,
        f"remotebuild-{name}",
        ",".join(users),
        REMOTEBUILD_VALIDITY,
        pubkey,
        serial=1,
        options=["clear"],
    )
    remote.write_file(target, f"{key_path}-cert.pub", cert)

    return f"{key_path}: provisioned (principals {','.join(users)})"


def provision_remotebuild_keys(name: str, meta: dict, force: bool) -> None:
    key_users = meta.get("remoteBuildKeyUsers", {})
    if not key_users:
        print(f"{name}: no remote-build keys declared for this host")
    for key_path, users in sorted(key_users.items()):
        print(
            f"{name}: " + provision_remotebuild_key(name, meta["sshTarget"], key_path, users, force)
        )


def renew_remotebuild_certs(name: str, meta: dict) -> None:
    key_paths = meta.get("remoteBuildKeys", [])
    if not key_paths:
        print(f"{name}: no remote-build keys declared for this host")
    for key_path in key_paths:
        print(f"{name}: {renew_remotebuild_cert(meta['sshTarget'], f'{key_path}-cert.pub')}")


def list_hosts(hosts: dict) -> None:
    for name in sorted(hosts):
        print(name)


def show(hosts: dict, host: str) -> None:
    print(json.dumps(require_host(hosts, host), indent=2))


def provision(hosts: dict, command: str, host: str, with_remotebuild: bool) -> None:
    meta = require_host(hosts, host)
    agents.unlock(HOST_CA_KEY)
    process_host(host, meta, force=command == "rotate")

    if with_remotebuild:
        agents.unlock(USER_CA_KEY)
        if command == "renew":
            renew_remotebuild_certs(host, meta)
        else:
            provision_remotebuild_keys(host, meta, force=command == "rotate")


def _each_host(hosts: dict, action) -> list[str]:
    failures = []
    for name in sorted(hosts):
        try:
            action(name, hosts[name])
        except subprocess.CalledProcessError:
            print(f"FAILED: {name}", file=sys.stderr)
            failures.append(name)
    return failures


def _report(failures: list[str], success: str) -> None:
    if failures:
        print(
            f"\nCompleted with {len(failures)} failure(s): {' '.join(failures)}",
            file=sys.stderr,
        )
        sys.exit(1)
    print(success)


def renew_all(hosts: dict, with_remotebuild: bool) -> None:
    agents.unlock(HOST_CA_KEY)
    if with_remotebuild:
        agents.unlock(USER_CA_KEY)

    def renew(name: str, meta: dict) -> None:
        process_host(name, meta, force=False)
        if with_remotebuild:
            renew_remotebuild_certs(name, meta)

    _report(_each_host(hosts, renew), "All hosts renewed successfully.")


def renew_remotebuild(hosts: dict, host: str) -> None:
    meta = require_host(hosts, host)
    agents.unlock(USER_CA_KEY)
    renew_remotebuild_certs(host, meta)


def renew_remotebuild_all(hosts: dict) -> None:
    agents.unlock(USER_CA_KEY)
    _report(
        _each_host(hosts, renew_remotebuild_certs),
        "Remotebuild key certs synced for all hosts.",
    )
