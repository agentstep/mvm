# In-guest checkpoint CLI — design

**Date:** 2026-08-21
**Status:** designed, not implemented. Everything else from the Sprites review
has shipped; this is the remaining item, and it needs infrastructure that does
not exist yet.

**Goal:** let an agent working inside a sandbox checkpoint and roll back its own
VM, the way Sprites expose `sprite-env` from inside the guest.

## Why this is not a small change

A VM cannot snapshot itself. The VMM does it from outside, so an in-guest
command has to reach the host — and **mvm has no guest→host channel at all**.
Every existing path runs host→guest: the daemon or CLI dials the agent, the
agent answers. Nothing in the guest can currently initiate anything.

That single fact is what makes this larger than the filesystem API or the policy
view, both of which reused channels that already existed.

## The channel

**Firecracker.** Its vsock device supports guest-initiated connections: the
guest connects to CID 2 on port N, and Firecracker connects to a Unix socket at
`<uds_path>_<N>` on the host. So the daemon listens on
`/run/mvm/<vm>.vsock_9000` and the guest dials CID 2 port 9000. The plumbing
already half-exists — `VsockUDSPath` gives the base path.

**applevz.** Different mechanism. `VZVirtioSocketDevice` supports guest→host
connections, but they surface through the Swift helper (`mvm-vz`), which would
need a listener and a way to forward the request to the CLI process that owns
the VM. That process may not even be running: on applevz there is no daemon, so
there may be nobody to answer. **This is the harder half, and it is why the
feature should ship Firecracker-first.**

## What the guest sends

A small binary, `mvm-env`, baked into the rootfs alongside `mvm-agent`:

    mvm-env checkpoint [name]     # snapshot this VM
    mvm-env checkpoint --list     # list this VM's snapshots
    mvm-env restore <name>        # roll back

It speaks the existing length-prefixed JSON framing, so no new wire format.

## The security question this raises

**Any process in the guest could call this.** The guest runs as root and hosts
untrusted agent code, so the channel is reachable by exactly the code the
sandbox exists to contain.

That is acceptable for checkpoint/restore specifically — the blast radius is the
VM's own state, which that code can already destroy by other means — but it is
**only** acceptable because the verb set is narrow. The channel must therefore:

- expose checkpoint, list and restore, and nothing else;
- never accept a VM name from the guest. The daemon knows which VM a connection
  came from by which socket it arrived on, exactly as the egress DNS proxy
  identifies a VM by which listener answered. A guest that could name its target
  could checkpoint or roll back *another* VM;
- rate-limit. Snapshots are cheap but not free, and a loop would fill the
  snapshot directory.

The last two are the reason this deserves a design rather than being bolted on.

## Interaction with restore

`restore` from inside is odd: the VM is replaced while the process that asked is
running in it. The request cannot be acknowledged, because the caller ceases to
exist mid-call. The guest client should treat a closed connection as success and
say so, rather than reporting an error on the one path that works correctly.

## Sequencing

1. Host listener on `<uds>_9000`, Firecracker only, checkpoint + list.
2. `mvm-env` in the rootfs, plus a line in `/AGENTS.md` so agents know it exists
   — that line is most of the value, since a capability no agent knows about
   goes unused.
3. Restore, with the disconnect-means-success handling above.
4. applevz, if the Swift helper work proves worth it.

## Why it was not built in this pass

The other Sprites items reused existing channels. This one adds a new inbound
attack surface reachable by untrusted guest code, and doing that carefully — VM
identity from the socket rather than the request, a deliberately narrow verb
set, rate limiting — is not work to rush at the end of a long session. The
design is here so it can be built deliberately.
