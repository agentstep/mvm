import Foundation
import Virtualization

struct VMConfig {
    let cpus: Int
    let memoryMB: Int
    let kernelPath: String
    let rootfsPath: String
    let bootArgs: String
    let macAddress: String?
    let logPath: String?
    let shares: [(tag: String, hostPath: String)]
}

enum VMError: Error, LocalizedError {
    case notRunning
    case notFound(String)
    case configError(String)

    var errorDescription: String? {
        switch self {
        case .notRunning: return "VM is not running"
        case .notFound(let name): return "VM '\(name)' not found"
        case .configError(let msg): return "Configuration error: \(msg)"
        }
    }
}

/// Builds VZVirtualMachineConfiguration from our VMConfig.
enum VMConfigBuilder {
    static func build(_ config: VMConfig) throws -> VZVirtualMachineConfiguration {
        let vzConfig = VZVirtualMachineConfiguration()

        vzConfig.cpuCount = config.cpus
        vzConfig.memorySize = UInt64(config.memoryMB) * 1024 * 1024

        // Linux boot loader
        let bootLoader = VZLinuxBootLoader(kernelURL: URL(fileURLWithPath: config.kernelPath))
        bootLoader.commandLine = config.bootArgs
        vzConfig.bootLoader = bootLoader

        // Root disk
        let diskAttachment = try VZDiskImageStorageDeviceAttachment(
            url: URL(fileURLWithPath: config.rootfsPath),
            readOnly: false
        )
        vzConfig.storageDevices = [VZVirtioBlockDeviceConfiguration(attachment: diskAttachment)]

        // Network (NAT)
        let networkDevice = VZVirtioNetworkDeviceConfiguration()
        networkDevice.attachment = VZNATNetworkDeviceAttachment()
        if let mac = config.macAddress {
            networkDevice.macAddress = VZMACAddress(string: mac) ?? .randomLocallyAdministered()
        }
        vzConfig.networkDevices = [networkDevice]

        // Serial console
        let serialPort = VZVirtioConsoleDeviceSerialPortConfiguration()
        if let logPath = config.logPath {
            FileManager.default.createFile(atPath: logPath, contents: nil)
            let logHandle = try FileHandle(forWritingTo: URL(fileURLWithPath: logPath))
            logHandle.seekToEndOfFile()
            serialPort.attachment = VZFileHandleSerialPortAttachment(
                fileHandleForReading: nil,
                fileHandleForWriting: logHandle
            )
        }
        vzConfig.serialPorts = [serialPort]

        // Entropy + memory balloon
        vzConfig.entropyDevices = [VZVirtioEntropyDeviceConfiguration()]
        vzConfig.memoryBalloonDevices = [VZVirtioTraditionalMemoryBalloonDeviceConfiguration()]

        // Vsock device — out-of-band control plane to the in-guest agent.
        // The Go side reaches the agent via the helper's IPC socket: the
        // helper calls VZVirtioSocketDevice.connect(toPort:) and ships the
        // resulting fd back via SCM_RIGHTS. With this device wired, the
        // agent listens on vsock port 5123 and the Go agentclient never
        // has to touch the guest's TAP IP.
        vzConfig.socketDevices = [VZVirtioSocketDeviceConfiguration()]

        // VirtioFS shared directories
        var dirSharingDevices: [VZDirectorySharingDeviceConfiguration] = []
        for share in config.shares {
            let sharedDir = VZSharedDirectory(url: URL(fileURLWithPath: share.hostPath), readOnly: false)
            let singleShare = VZSingleDirectoryShare(directory: sharedDir)
            let fsConfig = VZVirtioFileSystemDeviceConfiguration(tag: share.tag)
            fsConfig.share = singleShare
            dirSharingDevices.append(fsConfig)
        }
        if !dirSharingDevices.isEmpty {
            vzConfig.directorySharingDevices = dirSharingDevices
        }

        return vzConfig
    }
}
