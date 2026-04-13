import ArgumentParser
import Foundation

@main
struct MvmVZ: ParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "mvm-vz",
        abstract: "Manage lightweight Linux VMs using Apple Virtualization.framework",
        subcommands: [Create.self, Start.self, Stop.self, Status.self, Version.self]
    )
}

struct Version: ParsableCommand {
    static let configuration = CommandConfiguration(abstract: "Print version")

    func run() {
        print(#"{"version":"0.1.0","framework":"Virtualization.framework"}"#)
    }
}
