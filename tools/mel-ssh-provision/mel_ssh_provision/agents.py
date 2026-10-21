import contextlib
import os
import shutil
import signal
import subprocess
import sys
import tempfile

from .errors import ProvisionError


class CaAgents:
    """Ephemeral ssh-agents holding CA keys.

    Each agent is only ever reached through env(), never through this
    process's own SSH_AUTH_SOCK, so ssh logins to hosts keep using the
    invoking user's agent and can never offer a CA key.
    """

    def __init__(self) -> None:
        self._socks: dict[str, str] = {}
        self._pids: list[int] = []
        self._dirs: list[str] = []

    def unlock(self, ca_key: str) -> None:
        if ca_key in self._socks:
            return

        print(f"Unlocking CA key ({ca_key}) into ephemeral agent...", file=sys.stderr)

        directory = tempfile.mkdtemp()
        self._dirs.append(directory)
        sock = os.path.join(directory, "agent.sock")

        out = subprocess.run(
            ["ssh-agent", "-a", sock], check=True, capture_output=True, text=True
        ).stdout
        for line in out.splitlines():
            if line.startswith("SSH_AGENT_PID="):
                self._pids.append(int(line.split("=", 1)[1].split(";", 1)[0]))
                break
        else:
            raise ProvisionError("could not determine ssh-agent PID")

        with contextlib.ExitStack() as stack:
            tty = None
            if not sys.stdin.isatty():
                with contextlib.suppress(OSError):
                    tty = stack.enter_context(open("/dev/tty", "rb"))

            subprocess.run(
                ["ssh-add", ca_key],
                check=True,
                stdin=tty,
                stdout=subprocess.DEVNULL,
                env={**os.environ, "SSH_AUTH_SOCK": sock},
            )

        self._socks[ca_key] = sock

    def env(self, ca_key: str) -> dict[str, str]:
        self.unlock(ca_key)
        return {**os.environ, "SSH_AUTH_SOCK": self._socks[ca_key]}

    def close(self) -> None:
        for pid in self._pids:
            try:
                os.kill(pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
        for directory in self._dirs:
            shutil.rmtree(directory, ignore_errors=True)
        self._socks.clear()
        self._pids.clear()
        self._dirs.clear()


agents = CaAgents()
