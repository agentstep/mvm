// SCM_RIGHTS sender for the mvm-vz Swift helper.
//
// CMSG_FIRSTHDR / CMSG_DATA are macros that expand to pointer arithmetic
// on a fixed control buffer. They don't bridge cleanly to Swift, so the
// entire sendmsg-with-ancillary-fd dance lives here.

#include "MvmVZShim.h"

#include <errno.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/uio.h>
#include <unistd.h>

ssize_t mvm_send_fd(int socketFd, const void *payload, size_t payload_len, int passedFd) {
    if (payload == NULL || payload_len == 0) {
        // sendmsg with an empty iov is allowed but sendmsg+SCM_RIGHTS
        // semantics require at least one byte of payload to anchor the
        // ancillary message on the receiving side. Reject upfront.
        errno = EINVAL;
        return -1;
    }

    struct iovec iov;
    iov.iov_base = (void *)payload;
    iov.iov_len  = payload_len;

    // Control buffer sized for exactly one SCM_RIGHTS message carrying one int.
    char cmsg_buf[CMSG_SPACE(sizeof(int))];
    memset(cmsg_buf, 0, sizeof(cmsg_buf));

    struct msghdr msg;
    memset(&msg, 0, sizeof(msg));
    msg.msg_iov        = &iov;
    msg.msg_iovlen     = 1;
    msg.msg_control    = cmsg_buf;
    msg.msg_controllen = sizeof(cmsg_buf);

    struct cmsghdr *cmsg = CMSG_FIRSTHDR(&msg);
    if (cmsg == NULL) {
        errno = EINVAL;
        return -1;
    }
    cmsg->cmsg_level = SOL_SOCKET;
    cmsg->cmsg_type  = SCM_RIGHTS;
    cmsg->cmsg_len   = CMSG_LEN(sizeof(int));
    memcpy(CMSG_DATA(cmsg), &passedFd, sizeof(int));

    return sendmsg(socketFd, &msg, 0);
}
