import Darwin
import Foundation
import MvmVZShim

// IPCServer — per-VM Unix-domain socket listener.
//
// Each `mvm-vz create --foreground` process holds exactly one
// VZVirtualMachine. After the VM has started, the process spins up an
// IPCServer bound to ~/.mvm/run/vz-<name>.sock. The Go side
// (internal/vzhelper) connects to that socket, sends commands, and for
// the Connect command receives a vsock file descriptor via SCM_RIGHTS.
//
// The IPCServer holds a strong reference to the ManagedVM so the VM
// stays alive for the helper process's entire lifetime — there's no
// "VM ownership transfer" to think about.
//
// Threading
//
// All operations against the VZVirtualMachine MUST happen on a single
// dispatch queue (Apple-VF requires queue affinity). The IPCServer has
// its own accept queue for socket I/O, but every VZ method call is
// dispatched onto `vmQueue` so the VM object is only ever touched from
// one queue.

final class IPCServer {
    private let socketPath: String
    private let vm: ManagedVM
    private let acceptQueue = DispatchQueue(label: "mvm.vz.ipc.accept")

    // Protected by `lock`. `stop()` can be called from signal handlers,
    // VMDelegate callbacks, or the accept loop — all on different threads.
    private let lock = NSLock()
    private var listenFd: Int32 = -1
    private var stopped = false

    init(socketPath: String, vm: ManagedVM) {
        self.socketPath = socketPath
        self.vm = vm
    }

    /// Create the listening socket and start accepting connections in
    /// the background. Returns immediately on success.
    func start() throws {
        // Make sure the parent dir exists.
        let parent = (socketPath as NSString).deletingLastPathComponent
        try FileManager.default.createDirectory(
            atPath: parent,
            withIntermediateDirectories: true,
            attributes: nil
        )

        // Remove any stale socket from a previous run.
        unlink(socketPath)

        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        if fd < 0 {
            throw IPCError.ioError("socket(): errno=\(errno)")
        }

        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let pathBytes = Array(socketPath.utf8)
        if pathBytes.count >= MemoryLayout.size(ofValue: addr.sun_path) {
            close(fd)
            throw IPCError.ioError("socket path too long: \(socketPath)")
        }
        addr.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
        withUnsafeMutableBytes(of: &addr.sun_path) { buf in
            for (i, b) in pathBytes.enumerated() {
                buf[i] = b
            }
            buf[pathBytes.count] = 0
        }

        let bindResult = withUnsafePointer(to: &addr) { ptr -> Int32 in
            ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) { sap in
                bind(fd, sap, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        if bindResult != 0 {
            close(fd)
            throw IPCError.ioError("bind(): errno=\(errno)")
        }

        // 0600 permissions: only this user can talk to the helper.
        chmod(socketPath, 0o600)

        if listen(fd, 8) != 0 {
            close(fd)
            unlink(socketPath)
            throw IPCError.ioError("listen(): errno=\(errno)")
        }

        lock.lock()
        listenFd = fd
        lock.unlock()

        acceptQueue.async { [weak self] in
            self?.acceptLoop()
        }
    }

    /// Stop accepting new connections, close the listening socket, and
    /// remove the on-disk socket file.
    func stop() {
        lock.lock()
        let wasStopped = stopped
        stopped = true
        let fd = listenFd
        listenFd = -1
        lock.unlock()

        if wasStopped { return }
        if fd >= 0 {
            close(fd)
        }
        unlink(socketPath)
    }

    private var isStopped: Bool {
        lock.lock()
        defer { lock.unlock() }
        return stopped
    }

    private var currentListenFd: Int32 {
        lock.lock()
        defer { lock.unlock() }
        return listenFd
    }

    private func acceptLoop() {
        while !isStopped {
            let fd = currentListenFd
            if fd < 0 { return }

            let clientFd = accept(fd, nil, nil)
            if clientFd < 0 {
                if isStopped { return }
                // EINTR / EAGAIN — just retry.
                if errno == EINTR || errno == EAGAIN {
                    continue
                }
                fputs("[mvm-vz] accept failed: errno=\(errno)\n", stderr)
                return
            }

            // Set a read timeout so a hung or malicious client can't
            // hold a GCD thread forever.
            var tv = timeval(tv_sec: 5, tv_usec: 0)
            setsockopt(clientFd, SOL_SOCKET, SO_RCVTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))

            // Each client gets its own ephemeral queue. Connections are
            // short-lived (one request, one response, close).
            DispatchQueue.global(qos: .userInitiated).async { [weak self] in
                self?.handleClient(clientFd)
            }
        }
    }

    private func handleClient(_ fd: Int32) {
        let req: IPCRequest
        do {
            req = try IPCFraming.readRequest(fd)
        } catch {
            // Best-effort error response; ignore failures because the
            // peer may have already gone away.
            try? IPCFraming.writeResponse(fd, .error("read request: \(error)"))
            close(fd)
            return
        }

        switch req.cmd {
        case .status:
            vm.stateString { state in
                try? IPCFraming.writeResponse(fd, .okState(state))
                close(fd)
            }

        case .pause:
            vm.pause { result in
                let resp: IPCResponse
                switch result {
                case .success: resp = .ok()
                case .failure(let e): resp = .error("\(e)")
                }
                try? IPCFraming.writeResponse(fd, resp)
                close(fd)
            }

        case .resume:
            vm.resume { result in
                let resp: IPCResponse
                switch result {
                case .success: resp = .ok()
                case .failure(let e): resp = .error("\(e)")
                }
                try? IPCFraming.writeResponse(fd, resp)
                close(fd)
            }

        case .stop:
            vm.stop { result in
                let resp: IPCResponse
                switch result {
                case .success: resp = .ok()
                case .failure(let e): resp = .error("\(e)")
                }
                try? IPCFraming.writeResponse(fd, resp)
                close(fd)

                // After a successful Stop, the helper process exits. Give
                // the response a moment to flush, then bow out.
                if case .success = result {
                    DispatchQueue.global().asyncAfter(deadline: .now() + 0.1) {
                        Foundation.exit(0)
                    }
                }
            }

        case .connect:
            guard let port = req.port else {
                try? IPCFraming.writeResponse(fd, .error("missing 'port'"))
                close(fd)
                return
            }
            vm.openVsockConnection(port: port) { result in
                switch result {
                case .success(let vsockFd):
                    do {
                        // Send response + fd in one sendmsg via the C shim.
                        let payload = try IPCFraming.encodeFrame(.ok())
                        let sent = payload.withUnsafeBytes { (raw: UnsafeRawBufferPointer) -> Int in
                            guard let base = raw.baseAddress else { return -1 }
                            return mvm_send_fd(fd, base, payload.count, vsockFd)
                        }
                        if sent < 0 {
                            fputs("[mvm-vz] mvm_send_fd failed: errno=\(errno)\n", stderr)
                        }
                    } catch {
                        try? IPCFraming.writeResponse(fd, .error("encode: \(error)"))
                    }
                case .failure(let e):
                    try? IPCFraming.writeResponse(fd, .error("vsock connect: \(e)"))
                }
                close(fd)
            }
        }
    }
}
