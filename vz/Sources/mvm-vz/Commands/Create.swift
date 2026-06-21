import ArgumentParser
import Foundation
// Virtualization's types (VZVirtualMachine et al.) predate Sendable
// annotations. We confine all VM access to a single dispatch queue, so
// @preconcurrency suppresses the not-yet-Sendable diagnostics under Swift 6.
@preconcurrency import Virtualization

struct Create: ParsableCommand {
    static let configuration = CommandConfiguration(abstract: "Create and boot a VM")

    @Option(name: .long, help: "VM name")
    var name: String

    @Option(name: .long, help: "Path to Linux kernel")
    var kernel: String

    @Option(name: .long, help: "Path to rootfs ext4 image")
    var rootfs: String

    @Option(name: .long, help: "Number of CPUs")
    var cpus: Int = 1

    @Option(name: .long, help: "Memory in MB")
    var memory: Int = 128

    @Option(name: .long, help: "Kernel boot arguments")
    var bootArgs: String = "console=hvc0 reboot=k panic=1 quiet random.trust_cpu=on rootfstype=ext4"

    @Option(name: .long, help: "MAC address")
    var mac: String?

    @Option(name: .long, help: "Path for console log output")
    var logPath: String?

    @Option(name: .long, help: "Share host directory (hostPath:tag, repeatable)")
    var share: [String] = []

    @Option(name: .long, help: "Path to per-VM IPC socket (defaults to ~/.mvm/run/vz-<name>.sock)")
    var ipcSocket: String?

    @Flag(name: .long, help: "Run in foreground (block until VM stops)")
    var foreground: Bool = false

    func run() throws {
        // Parse share options into (tag, hostPath) tuples.
        // NOTE: the existing semantics (parts[0]=hostPath, parts[1]=tag)
        // match what internal/vm/applevz.go passes today. The volume-mount
        // feature is not yet end-to-end functional on either backend; see
        // the bonus-bug note in PR #1's commit message and the follow-up
        // issue for fixing virtiofs guest-side mount plumbing.
        var shares: [(tag: String, hostPath: String)] = []
        for s in share {
            let parts = s.split(separator: ":", maxSplits: 1)
            if parts.count == 2 {
                shares.append((tag: String(parts[1]), hostPath: String(parts[0])))
            }
        }

        let config = VMConfig(
            cpus: cpus,
            memoryMB: memory,
            kernelPath: kernel,
            rootfsPath: rootfs,
            bootArgs: bootArgs,
            macAddress: mac,
            logPath: logPath,
            shares: shares
        )

        // Build and start VM on a dedicated dispatch queue. VZ requires
        // that all VZVirtualMachine method calls happen on the queue the
        // machine was created on, so we make one queue here, use it for
        // the start callback, and hand it to ManagedVM for IPC dispatch.
        let vmQueue = DispatchQueue(label: "mvm.vz.vm.\(name)")

        let vzConfig = try VMConfigBuilder.build(config)
        try vzConfig.validate()

        // VZVirtualMachine is queue-affine: every method/property access must
        // happen on the queue the machine is bound to. The one-arg
        // VZVirtualMachine(configuration:) binds the machine to the MAIN queue,
        // so driving start()/pause()/vsock-connect from vmQueue tripped VZ's
        // dispatch_assert_queue (SIGTRAP). Use the queue-taking initializer so
        // the machine is bound to vmQueue — the same queue ManagedVM dispatches
        // all of its operations on — and invoke start() on that queue.
        let delegate = VMDelegate()
        _vmDelegateHolder = delegate // keep alive (machine.delegate is weak)
        let startSemaphore = DispatchSemaphore(value: 0)
        // Written only inside the start completion handler below and read only
        // after startSemaphore.wait(), which establishes the happens-before
        // ordering — hence nonisolated(unsafe) is sound.
        nonisolated(unsafe) var startError: Error?

        // Confined to vmQueue for its whole lifetime (created with that queue;
        // every access below and in ManagedVM is dispatched on it), so the
        // capture into the @Sendable closure is sound despite VZVirtualMachine
        // not being Sendable.
        nonisolated(unsafe) let machine = VZVirtualMachine(configuration: vzConfig, queue: vmQueue)

        vmQueue.async {
            machine.delegate = delegate
            machine.start { result in
                if case .failure(let error) = result {
                    startError = error
                }
                startSemaphore.signal()
            }
        }

        startSemaphore.wait()
        if let error = startError {
            throw error
        }

        // Wrap the running machine in ManagedVM and start the IPC server.
        let managed = try ManagedVM(name: name, machine: machine, vmQueue: vmQueue)

        let socketPath = ipcSocket ?? defaultIpcSocketPath(name: name)
        let ipcServer = IPCServer(socketPath: socketPath, vm: managed)
        try ipcServer.start()
        // Hold a strong reference so the server stays alive for the
        // process's entire lifetime.
        _ipcServerHolder = ipcServer

        let info: [String: Any] = [
            "name": name,
            "state": "running",
            "cpus": cpus,
            "memory_mb": memory,
            "ipc_socket": socketPath,
            "pid": ProcessInfo.processInfo.processIdentifier,
        ]
        let jsonData = try JSONSerialization.data(withJSONObject: info, options: [.sortedKeys])
        print(String(data: jsonData, encoding: .utf8)!)
        // Flush stdout so the Go side can parse the line immediately,
        // even if we're about to block in dispatchMain().
        fflush(stdout)

        if foreground {
            signal(SIGINT) { _ in
                _ipcServerHolder?.stop()
                Foundation.exit(0)
            }
            signal(SIGTERM) { _ in
                _ipcServerHolder?.stop()
                Foundation.exit(0)
            }
            dispatchMain()
        }
    }

    private func defaultIpcSocketPath(name: String) -> String {
        let home = FileManager.default.homeDirectoryForCurrentUser.path
        return "\(home)/.mvm/run/vz-\(name).sock"
    }
}

// File-scope strong reference so the IPCServer survives Create.run()
// returning into dispatchMain(). ARC would otherwise tear it down.
//
// `nonisolated(unsafe)` because Create.run() is not actor-isolated and
// signal handlers need to read this from arbitrary threads. Access is
// effectively single-writer (set once during boot) and read-only after.
nonisolated(unsafe) var _ipcServerHolder: IPCServer?

// File-scope strong reference to keep the VM delegate alive for the process
// lifetime — VZVirtualMachine.delegate is a weak reference, so a local would
// be deallocated once Create.run() returns into dispatchMain().
nonisolated(unsafe) var _vmDelegateHolder: VMDelegate?

// @unchecked Sendable: VMDelegate has no mutable stored state; its callbacks
// only stop the IPC server and exit. Safe to capture across queues.
final class VMDelegate: NSObject, VZVirtualMachineDelegate, @unchecked Sendable {
    func virtualMachine(_ virtualMachine: VZVirtualMachine, didStopWithError error: Error) {
        fputs("VM stopped with error: \(error)\n", stderr)
        _ipcServerHolder?.stop()
        Foundation.exit(1)
    }

    func guestDidStop(_ virtualMachine: VZVirtualMachine) {
        _ipcServerHolder?.stop()
        Foundation.exit(0)
    }
}
