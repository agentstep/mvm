package state

// GuestDocsPath is where the sandbox self-description is written inside the
// guest. AGENTS.md is the cross-tool convention; CLAUDE.md is symlinked to it
// so Claude Code picks it up without a second copy to keep in sync.
const (
	GuestDocsPath  = "/AGENTS.md"
	GuestDocsAlias = "/CLAUDE.md"
)

// GuestDocs is baked into the rootfs so an agent working inside a sandbox knows
// what the sandbox can do.
//
// Fly's Sprites do this and it is the cheapest capability on that list: without
// it an agent treats the VM as an ordinary Linux box, never checkpoints before
// a risky change, and cannot recover from its own mistakes. With it, the same
// agent can roll back.
//
// Deliberately short. This lands in the context window of every agent that runs
// here, so it earns its length or it gets ignored.
//
// NOTE: no apostrophes. This is written via a single-quoted heredoc inside the
// rootfs build script, where one would terminate the quote — the same failure
// that has already cost one full rootfs build to diagnose.
const GuestDocs = `# You are running inside an mvm microVM

This is a hardware-isolated sandbox, not a container. You have real root on a
real kernel. Destructive commands affect only this VM.

## What that means for you

**Break things freely.** The blast radius is this VM. You do not need to be
careful with the filesystem, packages, or the network in the way you would on a
host machine.

**Snapshot before anything risky.** The operator can restore a snapshot in well
under a second, so a snapshot is cheap insurance before a large refactor, a
dependency upgrade, or anything you are unsure of. Ask for one rather than
working around your uncertainty.

**Long-running processes should be services.** A dev server started with ` + "`&`" + `
dies when the VM restarts and nothing brings it back. A service is supervised
from outside this namespace: it is restarted if it crashes, survives a bounce,
and is replayed after a stop. Ask the operator to declare one with
` + "`mvm service add`" + ` rather than backgrounding a process yourself.

**A bounce is cheaper than a restart.** Restarting everything running in here
takes no reboot and keeps files, network and published ports intact. Prefer it
when you need a clean process state.

## What persists

Files persist across a bounce and across stop/start — the disk is the VMs
durable state. Processes, PTYs and shared memory do not survive a bounce.
Network configuration lives outside this namespace and is unaffected by one.

## What you cannot see

Your view of processes is namespaced: ` + "`ps`" + ` shows what runs in here, not the
whole guest. Memory and CPU figures come from the whole VM.

If network access fails for a host you expect to reach, the VM may have an
egress policy allowing only specific domains. That is enforced outside this VM
and you cannot change it from in here — say so rather than trying to work
around it.
`
