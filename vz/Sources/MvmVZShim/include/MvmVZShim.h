// MvmVZShim — C helpers that are awkward or impossible to express in
// pure Swift. Currently this is exclusively SCM_RIGHTS file-descriptor
// passing over Unix-domain sockets, which uses CMSG macros that don't
// bridge cleanly into Swift.
//
// All functions return the underlying syscall's return value, with errno
// preserved on failure. Callers should check the return code and read
// errno via Foundation.errno or Darwin.errno.

#ifndef MVMVZSHIM_H
#define MVMVZSHIM_H

#include <sys/types.h>

// mvm_send_fd writes (payload, payload_len) bytes on the SOCK_STREAM
// Unix-domain socket `socketFd`, attaching `passedFd` as ancillary data
// via SCM_RIGHTS in a single sendmsg() call.
//
// The receiving process must call recvmsg() (not read()) to obtain the
// fd; an ordinary read() will silently drop the ancillary message.
//
// On success returns the number of bytes written (always payload_len in
// practice on a stream socket where the payload fits in the kernel send
// buffer). On failure returns -1 with errno set.
ssize_t mvm_send_fd(int socketFd, const void *payload, size_t payload_len, int passedFd);

#endif /* MVMVZSHIM_H */
