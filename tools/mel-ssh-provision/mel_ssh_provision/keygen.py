import base64
import struct
import subprocess
import tempfile
from dataclasses import dataclass, field
from pathlib import Path

from .agents import agents
from .errors import ProvisionError
from .settings import DEFAULT_EXTENSIONS


def sign(
    ca_key: str,
    key_id: str,
    principals: str,
    validity: str,
    pubkey: bytes,
    *,
    host: bool = False,
    serial: int | None = None,
    options: list[str] = (),
) -> bytes:
    """Sign pubkey with the CA key held in its agent and return the certificate."""
    with tempfile.TemporaryDirectory() as tmp:
        pubfile = Path(tmp) / "key.pub"
        pubfile.write_bytes(pubkey)

        cmd = ["ssh-keygen", "-q", "-U", "-s", f"{ca_key}.pub", "-I", key_id]
        if host:
            cmd.append("-h")
        cmd += ["-n", principals, "-V", validity]
        if serial is not None:
            cmd += ["-z", str(serial)]
        for option in options:
            cmd += ["-O", option]
        cmd.append(str(pubfile))

        subprocess.run(cmd, check=True, env=agents.env(ca_key))

        certfile = Path(tmp) / "key-cert.pub"
        if not certfile.is_file():
            raise ProvisionError(f"certificate not generated: {certfile}")
        return certfile.read_bytes()


def fingerprint(pubkey_path: str) -> str:
    out = subprocess.run(
        ["ssh-keygen", "-l", "-f", pubkey_path],
        check=True,
        capture_output=True,
        text=True,
    ).stdout
    return out.split()[1]


@dataclass
class CertInfo:
    key_id: str = ""
    serial: int = 0
    principals: list[str] = field(default_factory=list)
    force_command: str | None = None
    other_critical: list[str] = field(default_factory=list)
    extensions: list[str] = field(default_factory=list)
    signing_ca: str | None = None


def describe(cert: bytes) -> CertInfo:
    """Parse `ssh-keygen -L` output for a certificate."""
    with tempfile.TemporaryDirectory() as tmp:
        certfile = Path(tmp) / "cert.pub"
        certfile.write_bytes(cert)
        text = subprocess.run(
            ["ssh-keygen", "-L", "-f", str(certfile)],
            check=True,
            capture_output=True,
            text=True,
        ).stdout

    info = CertInfo()
    options: list[str] = []
    section = None

    for raw in text.splitlines():
        line = raw.strip()

        if line.startswith("Signing CA:"):
            info.signing_ca = next((t for t in line.split() if t.startswith("SHA256:")), None)
            section = None
        elif "Key ID:" in line:
            info.key_id = line.split('"', 2)[1]
            section = None
        elif line.startswith("Serial:"):
            info.serial = int(line.split(":", 1)[1])
            section = None
        elif line.startswith("Principals:"):
            section = "principals"
        elif line.startswith("Critical Options:"):
            section = None if "(none)" in line else "critical"
        elif line.startswith("Extensions:"):
            section = None if "(none)" in line else "extensions"
        elif line.startswith("Valid:"):
            section = None
        elif not line:
            continue
        elif section == "principals":
            info.principals.append(line)
        elif section == "critical":
            name, _, value = line.partition(" ")
            if name == "force-command":
                info.force_command = value
            else:
                options.append(f"{name}={value}" if value else name)
        elif section == "extensions":
            options.append(line)

    info.extensions = [o for o in options if o in DEFAULT_EXTENSIONS]
    info.other_critical = [o for o in options if o not in DEFAULT_EXTENSIONS]
    return info


def policy_options(
    forced_command: str | None, extensions: list[str], critical: list[str] = ()
) -> list[str]:
    """ssh-keygen -O arguments for a policy.

    "clear" is needed because ssh-keygen otherwise adds its default extensions,
    and a forced command with no extensions must really mean none.
    """
    return [
        "clear",
        *([f"force-command={forced_command}"] if forced_command else []),
        *critical,
        *extensions,
    ]


def preserved_options(info: CertInfo) -> list[str]:
    return policy_options(info.force_command, info.extensions, info.other_critical)


def _read_wire_string(blob: bytes, offset: int) -> tuple[bytes, int]:
    (length,) = struct.unpack_from(">I", blob, offset)
    offset += 4
    return blob[offset : offset + length], offset + length


def _wire_string(data: bytes) -> bytes:
    return struct.pack(">I", len(data)) + data


def public_key_of(cert: bytes) -> str:
    """The plain public key line embedded in a certificate."""
    parts = cert.decode().splitlines()[0].strip().split(None, 2)
    if len(parts) < 2:
        raise ProvisionError("could not parse certificate public key line")

    blob = base64.b64decode(parts[1])
    wire_type, offset = _read_wire_string(blob, 0)
    wire_type = wire_type.decode()
    if not wire_type.endswith("-cert-v01@openssh.com"):
        raise ProvisionError(f"not a certificate public key (got type {wire_type})")

    _, offset = _read_wire_string(blob, offset)  # nonce

    if wire_type.startswith("ssh-ed25519-cert"):
        base_type, count = "ssh-ed25519", 1
    elif wire_type.startswith("ssh-rsa-cert"):
        base_type, count = "ssh-rsa", 2
    elif wire_type.startswith("ecdsa-sha2-") and "-cert" in wire_type:
        base_type, count = (
            "ecdsa-sha2-" + wire_type.split("ecdsa-sha2-")[1].split("-cert")[0],
            2,
        )
    else:
        raise ProvisionError(f"unsupported certificate key type: {wire_type}")

    out = _wire_string(base_type.encode())
    for _ in range(count):
        field_data, offset = _read_wire_string(blob, offset)
        out += _wire_string(field_data)

    return f"{base_type} {base64.b64encode(out).decode()}"
