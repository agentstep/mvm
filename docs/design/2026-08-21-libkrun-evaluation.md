# libkrun evaluation — decision: do not adopt

**Date:** 2026-08-21
**Question:** Should mvm embed [libkrun](https://github.com/libkrun/libkrun) as
its VMM, so a sandbox can live inside a host process rather than being driven as
a separate Firecracker/VZ process?

**Decision: no.** libkrun has no checkpoint/restore, and that is mvm's
differentiator.

## What libkrun is

A dynamic library that embeds a minimal VMM in the host process, using KVM on
Linux and Hypervisor.framework on macOS/ARM64. Rust, C API, Red Hat backed.
`crun` uses it to give containers VM-level isolation. It is the correct answer
to "I want a microVM inside my process" — which Firecracker is not, since
Firecracker ships as a binary driven over a REST socket with no supported
library form.

## Why not

**The C API has no snapshot.** `include/libkrun.h` declares 61 `krun_*`
functions. State management is exactly two of them:

- `krun_vm_pause` — freeze every vCPU at an instruction boundary
- `krun_vm_resume` — resume a paused VM

There is no checkpoint, snapshot, restore, or save. Pause/resume keeps a VM
resident in memory; it does not persist state to disk, and it cannot restore a
VM that has exited.

That is disqualifying here. mvm's snapshot story is a headline capability:
`mvm snapshot`/`mvm restore`, named snapshots, and sub-second fast-restore.
Fly's Sprites lead with ~300ms copy-on-write checkpoints. Moving to libkrun
would mean giving up the feature the product is positioned on, in exchange for
in-process embedding that mvm has no requirement for — the CLI/daemon split
already works, and a Rust or Go client over the existing daemon socket serves
any embedding need without touching the VMM.

## What was attractive, and is now moot

**TSI (transparent socket impersonation).** libkrun auto-enables it when no
network interface is added: guest sockets are impersonated by the host, so there
is no TAP device and no `CAP_NET_ADMIN`. That would have removed the privilege
requirement for unprivileged embedding, and it would have moved egress
enforcement from netfilter into host userspace.

**This is why the question was upstream of the egress plan.** Under TSI there is
no TAP interface to hang an iptables chain on, so the host-side egress plan
would have been targeting a layer that no longer existed.

With libkrun rejected, mvm keeps its own VMMs and their TAP devices, and **the
egress plan's approach is confirmed correct**: a per-VM nftables/iptables chain
on the guest's TAP interface, enforced outside the guest.

## Revisit if

- libkrun gains checkpoint/restore. This is the only blocker; everything else
  about it fits.
- mvm acquires a hard single-binary distribution constraint that rules out
  shipping a daemon.

Neither is true today.
