module_dir=$1
marker=/run/melnode-kmod-loaded

log() { echo "melnode-kmod: $*"; }

ko=$(find "$module_dir/lib/modules" -name 'melnode.ko*' -print -quit)
if [ -z "$ko" ]; then
  log "ERROR: no melnode.ko in $module_dir"
  exit 1
fi

load_deps() {
  {
    modinfo -F softdep "$ko" | sed -E 's/(pre|post)://g'
    modinfo -F depends "$ko" | tr ',' ' '
  } | tr -s '[:space:]' '\n' | while read -r dep; do
    [ -n "$dep" ] && modprobe -q "$dep" || true
  done
}

if [ -d /sys/module/melnode ]; then
  want=$(modinfo -F srcversion "$ko")
  have=$(cat /sys/module/melnode/srcversion 2>/dev/null || true)
  if { [ -n "$want" ] && [ "$want" = "$have" ]; } || [ "$(cat "$marker" 2>/dev/null)" = "$ko" ]; then
    log "already running this build, nothing to swap"
    exit 0
  fi

  running=$(uname -r)
  built_for=$(modinfo -F vermagic "$ko" | cut -d' ' -f1)
  if [ "$running" != "$built_for" ]; then
    log "WARNING: new module is built for kernel $built_for but $running is running; keeping the loaded module until reboot"
    exit 0
  fi

  log "swapping module: $have -> $want"
  tries=0
  until rmmod melnode 2>/dev/null; do
    tries=$((tries + 1))
    if [ "$tries" -ge 10 ]; then
      log "WARNING: could not unload melnode ($(rmmod melnode 2>&1)); keeping the loaded module"
      exit 0
    fi
    sleep 0.5
  done
fi

load_deps
if insmod "$ko"; then
  echo "$ko" >"$marker"
  log "loaded $ko"
  exit 0
fi

log "ERROR: insmod of $ko failed; falling back to the booted generation's module"
rm -f "$marker"
if modprobe melnode; then
  log "restored the booted generation's module"
  exit 0
fi
exit 1
