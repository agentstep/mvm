import ArgumentParser
import Foundation
import Virtualization

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

        var machine: VZVirtualMachine!
        vmQueue.sync {
            machine = VZVirtualMachine(configuration: vzConfig)
        }
        let delegate = VMDelegate()
        machine.delegate = delegate

        let startSemaphore = DispatchSemaphore(value: 0)
        var startError: Error?

        vmQueue.async {
            machine.start { result in
                switch result {
                case .success: break
                case .failure(let error): startError = error
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

class VMDelegate: NSObject, VZVirtualMachineDelegate {
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
