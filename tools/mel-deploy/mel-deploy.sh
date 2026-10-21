mapfile -t all_nodes < <(printf '%s\n' "${!fqdns[@]}" | sort)
known_nodes=$(printf '%s, ' "${all_nodes[@]}")
known_nodes=${known_nodes%, }

usage() {
  cat >&2 <<EOT
Melinoe deployment helper — drives update-safe over SSH.

Usage: mel-deploy <commit-message>

  [DEPLOY]         Deploy every node in the fleet.
  [DEPLOY <host>]  Deploy the named node only.

Known nodes: $known_nodes
EOT
  exit 1
}

commit_msg=${1:-}
[ -n "$commit_msg" ] || usage

deploy_all=false
targets=()

while IFS= read -r tag; do
  host=$(sed -E 's/^\[DEPLOY[[:space:]]*//; s/\]$//' <<<"$tag")
  if [ -z "$host" ]; then
    deploy_all=true
  else
    targets+=("$host")
  fi
done < <(grep -oP '\[DEPLOY(?:\s+[^\]]+)?\]' <<<"$commit_msg" || true)

if $deploy_all; then
  targets=("${all_nodes[@]}")
fi

if [ "${#targets[@]}" -eq 0 ]; then
  echo "ERROR: No [DEPLOY] tags found in commit message." >&2
  exit 1
fi

for host in "${targets[@]}"; do
  if [ -z "${fqdns[$host]:-}" ]; then
    echo "ERROR: Unknown node '$host'. Known nodes: $known_nodes" >&2
    exit 1
  fi
done

workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT
mkdir "$workdir/.ssh"

base64 -d <"${ssh_key:?}" >"$workdir/.ssh/id_ed25519"
base64 -d <"${ssh_cert:?}" >"$workdir/.ssh/id_ed25519-cert.pub"
base64 -d <"${ssh_host_ca:?}" >"$workdir/.ssh/host_ca.pub"
chmod 600 "$workdir/.ssh/id_ed25519"
printf '@cert-authority * %s\n' "$(cat "$workdir/.ssh/host_ca.pub")" >"$workdir/.ssh/known_hosts"

ssh_opts=(
  -n
  -i "$workdir/.ssh/id_ed25519"
  -o "CertificateFile=$workdir/.ssh/id_ed25519-cert.pub"
  -o "UserKnownHostsFile=$workdir/.ssh/known_hosts"
  -o StrictHostKeyChecking=yes
  -o BatchMode=yes
)

failed=()

for host in "${targets[@]}"; do
  echo "Deploying: $host → ${fqdns[$host]}"

  if ssh "${ssh_opts[@]}" -l gitlab-deploy "${fqdns[$host]}"; then
    echo "OK: $host"
  else
    echo "FAILED: $host" >&2
    failed+=("$host")
  fi
done

echo -e "\nDeployed ${#targets[@]} node(s), ${#failed[@]} failure(s)."

if [ "${#failed[@]}" -gt 0 ]; then
  echo "Failed nodes: ${failed[*]}" >&2
  exit 1
fi
