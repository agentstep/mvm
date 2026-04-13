import ArgumentParser
import Foundation

struct Stop: ParsableCommand {
    static let configuration = CommandConfiguration(abstract: "Stop a running VM by sending SIGTERM to its process")

    @Option(name: .long, help: "PID of the mvm-vz process managing the VM")
    var pid: Int32

    func run() throws {
        // Send SIGTERM to the mvm-vz process managing the VM
        // The signal handler in Create/Start will call manager.stop()
        let result = kill(pid, SIGTERM)
        if result != 0 {
            // Process may already be dead
            let result2 = kill(pid, SIGKILL)
            if result2 != 0 {
                print(#"{"status":"already_stopped"}"#)
                return
            }
        }
        print(#"{"status":"stopping"}"#)
    }
}
