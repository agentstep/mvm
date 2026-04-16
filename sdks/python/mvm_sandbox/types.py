"""Data types for the mvm sandbox SDK."""

from dataclasses import dataclass


@dataclass
class ExecResult:
    """Result of executing a command in a VM."""

    output: str
    exit_code: int


@dataclass
class VMInfo:
    """Information about a VM."""

    name: str
    status: str
    guest_ip: str = ""
    pid: int = 0
    created_at: str = ""


@dataclass
class SnapshotInfo:
    """Information about a snapshot."""

    name: str
    vm: str = ""
    created: str = ""
    type: str = ""


@dataclass
class ImageInfo:
    """Information about a custom rootfs image."""

    name: str
    size_mb: int = 0


@dataclass
class PoolStatus:
    """Status of the VM pool."""

    ready: int
    total: int


@dataclass
class PortMap:
    """Port mapping between host and guest."""

    host_port: int
    guest_port: int
    proto: str = "tcp"


@dataclass
class BuildStep:
    """A single build step (Dockerfile directive)."""

    directive: str
    args: str
