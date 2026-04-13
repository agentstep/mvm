import ArgumentParser
import Foundation

struct Status: ParsableCommand {
    static let configuration = CommandConfiguration(abstract: "Check if a VM process is running")

    @Option(name: .long, help: "PID of the mvm-vz process")
    var pid: Int32

    func run() {
        // Check if process is alive
        let result = kill(pid, 0)
        if result == 0 {
            print(#"{"state":"running","pid":\#(pid)}"#)
        } else {
            print(#"{"state":"stopped","pid":\#(pid)}"#)
        }
    }
}
