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

    @Flag(name: .long, help: "Run in foreground (block until VM stops)")
    var foreground: Bool = false

    func run() throws {
        // Parse share options into (tag, hostPath) tuples
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

        // Build and start VM synchronously using GCD
        let vzConfig = try VMConfigBuilder.build(config)
        try vzConfig.validate()

        let machine = VZVirtualMachine(configuration: vzConfig)
        let delegate = VMDelegate()
        machine.delegate = delegate

        let startSemaphore = DispatchSemaphore(value: 0)
        var startError: Error?

        machine.start { result in
            switch result {
            case .success: break
            case .failure(let error): startError = error
            }
            startSemaphore.signal()
        }

        startSemaphore.wait()
        if let error = startError {
            throw error
        }

        let info: [String: Any] = [
            "name": name,
            "state": "running",
            "cpus": cpus,
            "memory_mb": memory,
            "pid": ProcessInfo.processInfo.processIdentifier,
        ]
        let jsonData = try JSONSerialization.data(withJSONObject: info, options: [.sortedKeys])
        print(String(data: jsonData, encoding: .utf8)!)

        if foreground {
            signal(SIGINT) { _ in Foundation.exit(0) }
            signal(SIGTERM) { _ in Foundation.exit(0) }
            dispatchMain()
        }
    }
}

class VMDelegate: NSObject, VZVirtualMachineDelegate {
    func virtualMachine(_ virtualMachine: VZVirtualMachine, didStopWithError error: Error) {
        fputs("VM stopped with error: \(error)\n", stderr)
        Foundation.exit(1)
    }

    func guestDidStop(_ virtualMachine: VZVirtualMachine) {
        Foundation.exit(0)
    }
}
