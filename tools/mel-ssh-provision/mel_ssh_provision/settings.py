import os
from pathlib import Path

HOST_CA_KEY = os.environ.get("MEL_HOST_CA_KEY", str(Path.home() / "ssh-ca-keys/ssh-host-ca"))
HOST_VALIDITY = os.environ.get("MEL_HOST_CERT_VALIDITY", "+30d")

USER_CA_KEY = os.environ.get("MEL_USER_CA_KEY", str(Path.home() / "ssh-ca-keys/ssh-user-ca"))
USER_VALIDITY = os.environ.get("MEL_USER_CERT_VALIDITY", "+52w")

# Renewed unattended alongside host certs, so rotate on the host cadence rather than the human one.
REMOTEBUILD_VALIDITY = os.environ.get("MEL_REMOTEBUILD_CERT_VALIDITY", HOST_VALIDITY)

DEFAULT_EXTENSIONS = [
    "permit-X11-forwarding",
    "permit-agent-forwarding",
    "permit-port-forwarding",
    "permit-pty",
    "permit-user-rc",
]
