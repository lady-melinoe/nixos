#!/usr/bin/env -S ${EDITOR}

# this is jank, and WIP. add merge requests if u want lol.

Main: https://gitlab.melinoe.net/homelab/nixos

Mirror: https://gitlab.ucc.asn.au/melinoe/nixos-configs (via Gitlab Mirroring Push from Main)

Mirror 2: https://github.com/lady-melinoe/nixos (via Gitlab Mirroring Push from Main)

## Layout

| Path | |
| --- | --- |
| `flake/` | flake outputs: one `nixosConfiguration` per directory in `hosts/`, plus the apps |
| `cluster/` | cluster-wide data: nodes, VMs, SSH CAs, haproxy backends |
| `modules/` | every module, imported on every host; each file is one feature |
| `profiles/` | opt-in bundles a host lists in `imports`, as `profiles.<name>` |
| `hosts/<name>/` | what makes a node itself |
| `home/melinoe/` | melinoe's Home Manager config: `profiles/cli` and `profiles/gui` |
| `tools/` | `mel-deploy` and `mel-ssh-provision` (`nix run .#<name>`) |
| `assets/` | wallpaper and boot splash frames |
