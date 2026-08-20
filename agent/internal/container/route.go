package container

import "github.com/agentstep/mvm/agent/internal/protocol"

// insideVerbs are the request types handled in the inner container.
//
// Everything here is user code, or resolves paths that only exist in the
// container's mount view. Everything NOT here stays in the root namespace, and
// three of those exclusions are load-bearing rather than incidental:
//
//   - poweroff: reboot(2) from a non-initial PID namespace terminates the
//     namespace, not the machine. Routed inside, `mvm stop` would report success
//     while the VM kept running.
//   - tcp_forward: dials 127.0.0.1, and the netns is shared, so it reaches inner
//     services identically from outside. Keeping it outer also means its raw
//     unframed relay never has to cross the fd-passing boundary.
//   - setup_network / net_info: netlink and /proc/net are netns-scoped, and the
//     netns is shared, so behaviour is identical either side.
//
// file read/write DOES move inside: it must resolve targets under mounts that
// only exist in the container's view, such as /dev/shm or a volume.
var insideVerbs = map[string]bool{
	protocol.ReqExec:       true,
	protocol.ReqExecStream: true,
	protocol.ReqExecPty:    true,
	protocol.ReqWriteFile:  true,
	protocol.ReqReadFile:   true,
}

// RouteInside reports whether a request type is served by the inner container.
//
// Unknown verbs deliberately stay outside: forwarding one would hand the
// connection to an inner init that may not recognise it either, consuming the
// connection with no reply. The outer dispatch already returns a clear
// "unknown request type" error.
func RouteInside(reqType string) bool {
	return insideVerbs[reqType]
}
