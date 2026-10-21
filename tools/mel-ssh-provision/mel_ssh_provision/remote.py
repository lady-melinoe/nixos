import subprocess


def _ssh(target: str, *remote: str) -> list[str]:
    return ["ssh", f"root@{target}", *remote]


def run_script(
    target: str, script: str, *args: str, check: bool = True
) -> subprocess.CompletedProcess:
    return subprocess.run(
        _ssh(target, "bash", "-s", *(["--", *args] if args else [])),
        input=script.encode(),
        stdout=subprocess.PIPE,
        check=check,
    )


def write_file(target: str, path: str, data: bytes, then: str = "") -> None:
    command = f"cat > '{path}'" + (f" && {then}" if then else "")
    subprocess.run(_ssh(target, command), input=data, check=True)


HOST_KEY_SCRIPT = """set -euo pipefail

key=/etc/ssh/ssh_host_ed25519_key
pub=/etc/ssh/ssh_host_ed25519_key.pub

if [ "$1" = "true" ] || [ ! -s "$pub" ]; then
  rm -f "$key" "$pub"
  ssh-keygen -t ed25519 -N "" -f "$key" -q
fi

cat "$pub"
"""

REMOTEBUILD_KEY_SCRIPT = """set -euo pipefail

key="$1"
force="$2"

mkdir -p -m 700 "$(dirname "$key")"

if [ "$force" = "true" ] || [ ! -s "$key" ]; then
  rm -f "$key" "$key.pub" "$key-cert.pub"
  ssh-keygen -t ed25519 -N "" -C "" -f "$key" -q
fi

if [ ! -s "$key.pub" ]; then
  ssh-keygen -y -f "$key" > "$key.pub"
fi

cat "$key.pub"
"""

CERT_READ_SCRIPT = """set -euo pipefail
if [ -s "$1" ]; then
  cat "$1"
else
  exit 3
fi
"""
