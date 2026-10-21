usage() {
  cat <<'EOF'
Usage: update [--no-pull] [--no-verify] [--only-if-updated] [--boot] [--hostname HOSTNAME]

Options:
  --no-pull            Skip git pull in /etc/nixos
  --no-verify          Skip verification of the fetched commit signature
  --only-if-updated    Only rebuild if git merge changed the checkout
  --boot               Run nixos-rebuild boot instead of switch
  --hostname NAME      Rebuild a different host than the local machine
  -h, --help           Show this help
EOF
}

repo=/etc/nixos
no_pull=0
no_verify=0
only_if_updated=0
rebuild_mode=switch
hostname=$(hostname -s)

while [ "$#" -gt 0 ]; do
  case "$1" in
    --no-pull) no_pull=1 ;;
    --no-verify) no_verify=1 ;;
    --only-if-updated) only_if_updated=1 ;;
    --boot) rebuild_mode=boot ;;
    --hostname)
      [ "$#" -ge 2 ] || {
        echo "--hostname requires a value" >&2
        exit 1
      }
      hostname=$2
      shift
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
  shift
done

if [ "$no_pull" -eq 0 ]; then
  upstream=$(git -C "$repo" rev-parse --abbrev-ref --symbolic-full-name '@{u}')
  before=$(git -C "$repo" rev-parse HEAD)

  git -C "$repo" fetch
  upstream_commit=$(git -C "$repo" rev-parse --verify "$upstream^{commit}")

  if [ "$no_verify" -eq 0 ]; then
    git -C "$repo" -c "gpg.ssh.allowedSignersFile=$repo/allowed_signers" verify-commit "$upstream_commit"
  fi

  git -C "$repo" merge --ff-only "$upstream_commit"

  if [ "$only_if_updated" -eq 1 ] && [ "$before" = "$(git -C "$repo" rev-parse HEAD)" ]; then
    echo "No changes pulled; skipping rebuild."
    exit 0
  fi
fi

exec nixos-rebuild "$rebuild_mode" --flake "$repo/#$hostname"
