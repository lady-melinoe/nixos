{
  config,
  lib,
  pkgs,
  ...
}:
let
  inherit (lib) mkOption mkIf types;
  rtsCfg = config.melinoe.services.melinoe-rts;
  routeCfg = config.melinoe.services.melinoe-route;

  # Shared with melinoe-route.nix's rts-integration wiring -- must match
  # the path used there. Lives under RuntimeDirectory so systemd owns
  # its lifecycle (created before ExecStart, cleaned up on stop) instead
  # of us managing a stale socket file by hand.
  controlSocketPath = "/run/melinoe-rts/control.sock";

  # config.boot.kernelPackages.kernel.dev/vmlinux is the uncompressed
  # vmlinux nixpkgs's own kernel builder produces (manual-config.nix's
  # postInstall does `cp vmlinux $dev/`) -- BTF-bearing because
  # nixpkgs's common-config.nix sets DEBUG_INFO_BTF=y for any kernel
  # >=5.11, unconditionally, so no config override is needed for
  # pkgs.linuxPackages_latest as used in common/settings.nix. Deriving
  # vmlinux.h from this instead of a committed file means it's pinned to
  # exactly the kernel every fleet member actually boots (same
  # flake.lock => same kernel derivation everywhere), and a kernel bump
  # regenerates it automatically instead of relying on someone
  # remembering to re-run bpftool on some dev box.
  #
  # Also dumps nf_conntrack module BTF and merges it in, for
  # ct_redirect.bpf.c's kernel-conntrack-sync section (struct nf_conn,
  # struct bpf_ct_opts, bpf_skb_ct_lookup/bpf_ct_change_status/
  # bpf_ct_release -- all defined in the nf_conntrack module, not core
  # vmlinux). The module lives at
  # ${config.boot.kernelPackages.kernel.modules}, a separate output of
  # the kernel derivation from `out`/`dev` (this kernel's `out` doesn't
  # carry lib/modules at all). DEBUG_INFO_BTF_MODULES bakes split BTF
  # directly into each .ko, so this dumps straight from the compiled
  # module file, no live loaded module required; -B resolves the split
  # BTF's references back into core vmlinux's types.
  #
  # Open question we're deliberately not pre-resolving here: whether
  # `bpftool btf dump ... format c` actually reconstructs the kfunc
  # `extern` prototypes and struct bpf_ct_opts from this, or omits them
  # the way an earlier attempt appeared to (struct bpf_ct_opts came
  # back with no declaration at all, not even a forward decl, in that
  # attempt -- possibly because bpftool's C dumper drops types only
  # reachable through a kfunc's FUNC_PROTO param list, since it never
  # prints FUNC entries themselves). ct_redirect.bpf.c is written
  # assuming this works (relies on vmlinux.h directly, no hand-declared
  # fallback) -- if the build fails with undeclared-kfunc errors again,
  # that theory holds and hand-declaring is the fix, same as before.
  vmlinuxH =
    pkgs.runCommand "melinoe-rts-vmlinux.h"
      {
        nativeBuildInputs = [
          pkgs.bpftools
          pkgs.zstd
          pkgs.xz
        ];
      }
      ''
        bpftool btf dump file ${config.boot.kernelPackages.kernel.dev}/vmlinux format c > core.h

        ko=$(find ${config.boot.kernelPackages.kernel.modules}/lib/modules -iname 'nf_conntrack.ko*' | head -1)
        if [ -z "$ko" ]; then
          echo "melinoe-rts-vmlinux.h: couldn't find nf_conntrack.ko under ${config.boot.kernelPackages.kernel.modules}/lib/modules -- is CONFIG_NF_CONNTRACK enabled?" >&2
          exit 1
        fi
        case "$ko" in
          *.zst) zstd -d "$ko" -o nf_conntrack.ko ;;
          *.xz)  unxz -c "$ko" > nf_conntrack.ko ;;
          *.gz)  gzip -dc "$ko" > nf_conntrack.ko ;;
          *)     cp "$ko" nf_conntrack.ko ;;
        esac
        bpftool btf dump file nf_conntrack.ko format c -B ${config.boot.kernelPackages.kernel.dev}/vmlinux > nf_conntrack.h

        cat core.h nf_conntrack.h > $out
      '';

  melinoeRtsBinary = pkgs.buildGoModule {
    pname = "melinoe-rts";
    version = "0.0.1";

    src = ./melinoe-rts-daemon-src;
    vendorHash = "sha256-83YSPTT2HRVZELTNIpsYUnvcrJaJM7xt6FgNXpylhCs=";

    # go.mod declares bpf2go via a `tool` line specifically so it's part
    # of the module graph despite never being imported by a package here
    # (only invoked through go:generate). buildGoModule's default `go mod
    # vendor` path doesn't reliably mark tool-directive deps as explicit
    # in the vendor/modules.txt it generates ("is explicitly required in
    # go.mod, but not marked as explicit in vendor/modules.txt").
    # proxyVendor fetches via `go mod download` and proxies the vendor
    # dir instead of `go mod vendor`-ing it, which nixpkgs docs call out
    # as the fix for this -- also generally recommended whenever C
    # sources are involved, as they are here via bpf/ct_redirect.bpf.c.
    proxyVendor = true;

    # libbpf is here for its headers only (bpf/bpf_helpers.h etc, pulled
    # in by ct_redirect.bpf.c) -- nothing links against it, the compiled
    # BPF object is fully self-contained once bpf2go embeds it.
    #
    # This is deliberately NOT wired in via buildInputs/nativeBuildInputs
    # + cc-wrapper's automatic include-path hook: like buildInputs above,
    # that whole mechanism turned out not to reach proxyVendor's separate
    # go-modules fetcher derivation at all (confirmed by the derivation's
    # output path not changing when libbpf was added to buildInputs).
    #
    # It's also NOT wired in via BPF2GO_CFLAGS (tried that first) --
    # despite what the env-var name suggests, bpf2go doesn't read it
    # itself; it's purely a shell-substitution convention that only does
    # anything if the go:generate directive's own -cflags string
    # literally references "$BPF2GO_CFLAGS", which ours doesn't. Setting
    # it was a silent no-op.
    #
    # CPATH, by contrast, is read directly by clang/gcc as an extra
    # system include search path regardless of what's on the command
    # line, so it doesn't depend on anything in main.go's go:generate
    # line cooperating -- and being set inside preBuild (which nixpkgs'
    # buildGoModule does forward to both derivations) with the store path
    # interpolated by Nix at eval time, it reaches the actual compile.
    nativeBuildInputs = [
      pkgs.clang
      pkgs.llvm
    ];

    # Nix's clang wrapper injects hardening flags (-fstack-protector-strong,
    # -fzero-call-used-regs=used-gpr, ...) into every invocation by
    # default; clang rejects several of them outright for BPF targets
    # ("unsupported option ... for target 'bpfel'"), which breaks bpf2go's
    # compile step during `go generate`.
    #
    # This can't be `hardeningDisable` -- proxyVendor's `go-modules`
    # fetcher derivation is a *separate* derivation, and nixpkgs'
    # buildGoModule only forwards a specific attribute whitelist to it
    # (preBuild among them, which is why `go generate` -- and therefore
    # this clang invocation -- runs there too). `hardeningDisable` isn't
    # on that whitelist, but `env` is, so set the hardening override as
    # an env var directly to make sure it reaches both derivations.
    env.NIX_HARDENING_ENABLE = "";

    preBuild = ''
      cp ${vmlinuxH} bpf/vmlinux.h
      export CPATH="${pkgs.libbpf}/include"
      go generate ./...
    '';

    postInstall = ''
      mv $out/bin/ctdemo $out/bin/melinoe-rts
    '';
  };
in
{
  options.melinoe.services.melinoe-rts = {
    enabled = mkOption {
      type = types.bool;
      default = false;
      description = "Enable the Melinoe RTS (return-to-sender) eBPF daemon.";
    };

    prefixMatch = mkOption {
      type = types.listOf types.str;
      default = [ ];
      description = ''
        Interface name prefixes to mark as RTS by name match (the
        daemon's --watch-prefix). Additive with whatever melinoe-route
        adds over the control socket when
        melinoe.services.melinoe-route.rts-integration is enabled -- an
        interface matching either source is treated as RTS. Can be left
        empty if rts-integration is doing all the work.
      '';
    };
  };

  config = mkIf rtsCfg.enabled {
    assertions = [
      {
        assertion = rtsCfg.prefixMatch != [ ] || routeCfg.rts-integration;
        message = "melinoe.services.melinoe-rts.enabled needs at least one source of RTS interfaces: either melinoe.services.melinoe-rts.prefixMatch non-empty, or melinoe.services.melinoe-route.rts-integration = true.";
      }
    ];

    # ct_redirect.bpf.c's kernel-conntrack-sync section calls
    # bpf_skb_ct_lookup, a kfunc owned by the nf_conntrack module --
    # its BTF ID is only registered with the kernel once the module is
    # actually loaded (register_btf_kfunc_id_set() runs from the
    # module's own init, not at kernel boot). Nothing else on this
    # host is guaranteed to have triggered that load before
    # melinoe-rts starts (no iptables/nftables ct rule may have run
    # yet this early in boot), so the loader would otherwise
    # intermittently fail with "kfunc bpf_skb_ct_lookup: not
    # supported" depending on boot-order luck. boot.kernelModules is
    # loaded by systemd-modules-load.service, which orders before
    # multi-user.target (and therefore before this service, which is
    # wantedBy multi-user.target) unconditionally, every boot.
    boot.kernelModules = [ "nf_conntrack" ];

    systemd.services.melinoe-rts = {
      description = "Melinoe RTS (return-to-sender) eBPF daemon";
      wantedBy = [ "multi-user.target" ];
      after = [
        "network.target"
        "systemd-modules-load.service"
      ];
      stopIfChanged = false;
      serviceConfig = {
        ExecStart =
          "${melinoeRtsBinary}/bin/melinoe-rts"
          + lib.optionalString (rtsCfg.prefixMatch != [ ]) (
            " --watch-prefix ${lib.concatStringsSep "," rtsCfg.prefixMatch}"
          )
          + " --control-socket ${controlSocketPath}";
        Restart = "always";
        RuntimeDirectory = "melinoe-rts";
        # CAP_NET_ADMIN: attaching tcx programs to interfaces.
        # CAP_BPF: loading BPF programs/maps (5.8+ split this out of
        # CAP_SYS_ADMIN).
        # CAP_SYS_RESOURCE: only actually needed pre-~5.11 for the
        # RLIMIT_MEMLOCK bump the daemon does defensively on startup;
        # harmless to keep on newer kernels.
        AmbientCapabilities = [
          "CAP_NET_ADMIN"
          "CAP_BPF"
          "CAP_SYS_RESOURCE"
          "CAP_PERFMON"
        ];
        CapabilityBoundingSet = [
          "CAP_NET_ADMIN"
          "CAP_BPF"
          "CAP_SYS_RESOURCE"
          "CAP_PERFMON"
        ];
      };
    };
  };
}
