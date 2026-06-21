import ArgumentParser
import Foundation
@preconcurrency import Virtualization

// Spike (Tier 2 P1 go/no-go): does VZ's save/restore API accept our device set,
// specifically WITH the vsock control plane that the agent needs? Reports
// validateSaveRestoreSupport() for the full device set and the trimmed
// save/restore set (entropy/console/balloon removed, vsock + network kept).
struct ValidateSaveRestore: ParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "validate-saverestore",
        abstract: "Report VZ validateSaveRestoreSupport() for the full and save/restore device sets")

    @Option(name: .long, help: "Path to Linux kernel") var kernel: String
    @Option(name: .long, help: "Path to rootfs ext4 image") var rootfs: String

    func run() throws {
        let base = VMConfig(
            cpus: 2, memoryMB: 1024, kernelPath: kernel, rootfsPath: rootfs,
            bootArgs: "console=hvc0 root=/dev/vda rw", macAddress: "06:00:AC:10:00:01",
            logPath: "/tmp/mvm-vsr-spike.log", shares: [])

        for (label, sr) in [("FULL device set", false),
                            ("save/restore set (no entropy/console/balloon; keeps vsock + NAT)", true)] {
            let cfg = try VMConfigBuilder.build(base, saveRestore: sr)
            do {
                try cfg.validate()
            } catch {
                print("[\(label)] validate() failed: \(error.localizedDescription)")
                continue
            }
            do {
                try cfg.validateSaveRestoreSupport()
                print("[\(label)] validateSaveRestoreSupport(): SUPPORTED ✅")
            } catch {
                print("[\(label)] validateSaveRestoreSupport(): REJECTED ❌ — \(error.localizedDescription)")
            }
        }
    }
}
