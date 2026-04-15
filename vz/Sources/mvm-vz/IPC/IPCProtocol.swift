import Foundation

// IPCProtocol — the wire format for the per-VM helper IPC socket.
//
// All messages are length-prefixed JSON: a 4-byte big-endian length
// followed by the JSON payload. The Go side (internal/vzhelper) speaks
// the exact same convention. See Go internal/vzhelper/protocol.go for
// the canonical definitions; this file mirrors them in Swift so that
// either side can be modified without code generation.

enum IPCCommand: String {
    case connect
    case pause
    case resume
    case stop
    case status
}

struct IPCRequest {
    let cmd: IPCCommand
    let port: UInt32?

    static func decode(_ data: Data) throws -> IPCRequest {
        guard let obj = try JSONSerialization.jsonObject(with: data) as? [String: Any] else {
            throw IPCError.malformedRequest("not a JSON object")
        }
        guard let cmdStr = obj["cmd"] as? String, let cmd = IPCCommand(rawValue: cmdStr) else {
            throw IPCError.malformedRequest("missing or unknown 'cmd'")
        }
        var port: UInt32? = nil
        if let p = obj["port"] as? Int {
            port = UInt32(p)
        } else if let p = obj["port"] as? UInt32 {
            port = p
        }
        return IPCRequest(cmd: cmd, port: port)
    }
}

struct IPCResponse {
    let ok: Bool
    let error: String?
    let state: String?

    static func ok() -> IPCResponse {
        return IPCResponse(ok: true, error: nil, state: nil)
    }

    static func okState(_ state: String) -> IPCResponse {
        return IPCResponse(ok: true, error: nil, state: state)
    }

    static func error(_ message: String) -> IPCResponse {
        return IPCResponse(ok: false, error: message, state: nil)
    }

    func encode() throws -> Data {
        var obj: [String: Any] = ["ok": ok]
        if let e = error { obj["error"] = e }
        if let s = state { obj["state"] = s }
        return try JSONSerialization.data(withJSONObject: obj, options: [.sortedKeys])
    }
}

enum IPCError: Error, LocalizedError {
    case malformedRequest(String)
    case ioError(String)
    case shortRead
    case frameTooLarge(UInt32)

    var errorDescription: String? {
        switch self {
        case .malformedRequest(let m): return "malformed request: \(m)"
        case .ioError(let m):           return "io error: \(m)"
        case .shortRead:                return "short read"
        case .frameTooLarge(let n):     return "frame too large: \(n) bytes"
        }
    }
}

// IPCFraming — length-prefixed JSON frame I/O over a raw socket fd.
//
// We use BSD sockets directly (via Darwin) rather than Foundation's
// FileHandle or Network.framework because IPCServer needs sendmsg() with
// SCM_RIGHTS ancillary data for fd passing, and those frameworks abstract
// away the underlying socket. Keeping everything on raw fds means
// IPCFraming and the SCM_RIGHTS path use one consistent transport.
enum IPCFraming {
    static let maxFrameSize: UInt32 = 1 * 1024 * 1024 // 1 MiB

    /// Read exactly `count` bytes from fd, blocking until satisfied.
    /// Returns nil on EOF before the count is met.
    static func readExactly(_ fd: Int32, count: Int) -> Data? {
        var buf = Data(count: count)
        var offset = 0
        while offset < count {
            let n = buf.withUnsafeMutableBytes { (rawBuf: UnsafeMutableRawBufferPointer) -> Int in
                guard let base = rawBuf.baseAddress else { return -1 }
                return read(fd, base.advanced(by: offset), count - offset)
            }
            if n < 0 {
                if errno == EINTR { continue }
                return nil
            }
            if n == 0 {
                return nil // true EOF
            }
            offset += n
        }
        return buf
    }

    /// Read a length-prefixed JSON frame and decode it as IPCRequest.
    static func readRequest(_ fd: Int32) throws -> IPCRequest {
        guard let hdr = readExactly(fd, count: 4) else {
            throw IPCError.shortRead
        }
        let size: UInt32 = hdr.withUnsafeBytes { (raw: UnsafeRawBufferPointer) -> UInt32 in
            let b0 = UInt32(raw[0])
            let b1 = UInt32(raw[1])
            let b2 = UInt32(raw[2])
            let b3 = UInt32(raw[3])
            return (b0 << 24) | (b1 << 16) | (b2 << 8) | b3
        }
        if size > maxFrameSize {
            throw IPCError.frameTooLarge(size)
        }
        guard let body = readExactly(fd, count: Int(size)) else {
            throw IPCError.shortRead
        }
        return try IPCRequest.decode(body)
    }

    /// Encode `resp` as a length-prefixed JSON frame.
    static func encodeFrame(_ resp: IPCResponse) throws -> Data {
        let body = try resp.encode()
        var hdr = Data(count: 4)
        let size = UInt32(body.count)
        hdr[0] = UInt8((size >> 24) & 0xff)
        hdr[1] = UInt8((size >> 16) & 0xff)
        hdr[2] = UInt8((size >> 8)  & 0xff)
        hdr[3] = UInt8(size         & 0xff)
        return hdr + body
    }

    /// Write a length-prefixed JSON response frame to fd. Used for non-Connect
    /// responses (no SCM_RIGHTS).
    static func writeResponse(_ fd: Int32, _ resp: IPCResponse) throws {
        let frame = try encodeFrame(resp)
        var offset = 0
        while offset < frame.count {
            let n = frame.withUnsafeBytes { (rawBuf: UnsafeRawBufferPointer) -> Int in
                guard let base = rawBuf.baseAddress else { return -1 }
                return write(fd, base.advanced(by: offset), frame.count - offset)
            }
            if n < 0 {
                if errno == EINTR { continue }
                throw IPCError.ioError("write returned \(n), errno=\(errno)")
            }
            if n == 0 {
                throw IPCError.ioError("write returned 0")
            }
            offset += n
        }
    }
}
