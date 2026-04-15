import Foundation
import Virtualization

// ManagedVM wraps a VZVirtualMachine with a single dedicated dispatch
// queue, so all framework calls happen on one thread and the IPC server
// can call into the VM from any goroutine-equivalent without violating
// VZ's queue affinity rules.
//
// All methods are async-style (callback) because the underlying VZ
// methods are themselves asynchronous and must be issued from `vmQueue`.
//
// IPCServer holds a strong reference to ManagedVM, and the Create
// command holds a strong reference to IPCServer, so the chain stays
// alive for the helper process's entire lifetime.

final class ManagedVM {
    let name: String
    private let machine: VZVirtualMachine
    private let socketDevice: VZVirtioSocketDevice
    private let vmQueue: DispatchQueue

    // Strong references to in-flight vsock connections. Apple's framework
    // documentation says a VZVirtioSocketConnection keeps the underlying
    // file alive only as long as the connection object exists; we hold
    // them so that fds passed to the Go side via SCM_RIGHTS stay valid.
    //
    // Periodically pruned to avoid unbounded growth on long-lived VMs.
    private var heldConnections: [VZVirtioSocketConnection] = []
    private let heldLock = NSLock()
    private let pruneThreshold = 64

    init(name: String, machine: VZVirtualMachine, vmQueue: DispatchQueue) throws {
        self.name = name
        self.machine = machine
        self.vmQueue = vmQueue

        // Find the vsock device. VMConfigBuilder always wires exactly one.
        guard let dev = machine.socketDevices.first as? VZVirtioSocketDevice else {
            throw VMError.configError("VM has no VZVirtioSocketDevice — vsock not configured")
        }
        self.socketDevice = dev
    }

    /// Returns the current VM state as the lowercase string the Go side
    /// expects: "running", "paused", "stopped", "error", or "starting".
    /// Dispatches to vmQueue to satisfy VZ's queue-affinity requirement.
    func stateString(_ completion: @escaping (String) -> Void) {
        vmQueue.async {
            let s: String
            switch self.machine.state {
            case .running:    s = "running"
            case .paused:     s = "paused"
            case .stopped:    s = "stopped"
            case .error:      s = "error"
            case .starting:   s = "starting"
            case .pausing:    s = "pausing"
            case .resuming:   s = "resuming"
            case .stopping:   s = "stopping"
            case .saving:     s = "saving"
            case .restoring:  s = "restoring"
            @unknown default: s = "unknown"
            }
            completion(s)
        }
    }

    /// Pause the VM. On VZ this freezes vCPUs and keeps memory resident.
    func pause(_ completion: @escaping (Result<Void, Error>) -> Void) {
        vmQueue.async {
            self.machine.pause { result in
                switch result {
                case .success:        completion(.success(()))
                case .failure(let e): completion(.failure(e))
                }
            }
        }
    }

    /// Resume a previously paused VM.
    func resume(_ completion: @escaping (Result<Void, Error>) -> Void) {
        vmQueue.async {
            self.machine.resume { result in
                switch result {
                case .success:        completion(.success(()))
                case .failure(let e): completion(.failure(e))
                }
            }
        }
    }

    /// Stop the VM. The helper process exits shortly afterward.
    func stop(_ completion: @escaping (Result<Void, Error>) -> Void) {
        vmQueue.async {
            do {
                try self.machine.requestStop()
                completion(.success(()))
            } catch {
                completion(.failure(error))
            }
        }
    }

    /// Open a vsock connection from the host to the in-guest agent on
    /// `port` and return the resulting file descriptor. The fd is owned
    /// by the underlying VZVirtioSocketConnection; ManagedVM holds a
    /// strong reference to that connection so the fd stays valid even
    /// after the receiver dups it via SCM_RIGHTS.
    func openVsockConnection(port: UInt32, completion: @escaping (Result<Int32, Error>) -> Void) {
        vmQueue.async {
            self.socketDevice.connect(toPort: port) { result in
                switch result {
                case .success(let conn):
                    self.holdConnection(conn)
                    completion(.success(conn.fileDescriptor))
                case .failure(let e):
                    completion(.failure(e))
                }
            }
        }
    }

    /// Hold a connection reference and prune stale entries if the list
    /// exceeds the threshold. VZVirtioSocketConnection doesn't expose a
    /// "closed" property, but fileDescriptor returns -1 after the
    /// connection is torn down.
    private func holdConnection(_ conn: VZVirtioSocketConnection) {
        heldLock.lock()
        heldConnections.append(conn)
        if heldConnections.count > pruneThreshold {
            heldConnections.removeAll { $0.fileDescriptor < 0 }
        }
        heldLock.unlock()
    }
}
