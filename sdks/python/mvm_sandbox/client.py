"""Client for the mvm sandbox daemon REST API."""

from __future__ import annotations

import os
import re
from typing import List, Optional

import httpx

from .exceptions import AuthError, ConflictError, MVMError, NotFoundError, ServerError
from .types import (
    BuildStep,
    ExecResult,
    ImageInfo,
    PoolStatus,
    SnapshotInfo,
    VMInfo,
)


def _default_socket_path() -> str:
    """Return the default daemon Unix socket path."""
    home = os.path.expanduser("~")
    # Check Lima-forwarded socket first.
    lima_sock = os.path.join(home, ".lima", "mvm", "sock", "daemon.sock")
    if os.path.exists(lima_sock):
        return lima_sock
    return os.path.join(home, ".mvm", "server.sock")


def _parse_dockerfile(content: str) -> List[BuildStep]:
    """Parse a Dockerfile string into BuildStep list.

    Supports RUN, ENV, COPY, and ADD directives. Lines starting with #
    are treated as comments and skipped. Continuation lines (ending with
    backslash) are joined.
    """
    steps: List[BuildStep] = []
    supported = {"RUN", "ENV", "COPY", "ADD"}
    lines = content.splitlines()

    i = 0
    while i < len(lines):
        line = lines[i].strip()
        i += 1

        if not line or line.startswith("#"):
            continue

        # Handle continuation lines.
        while line.endswith("\\") and i < len(lines):
            line = line[:-1] + lines[i]
            i += 1

        # Match directive at the beginning of the line.
        match = re.match(r"^([A-Z]+)\s+(.*)", line, re.DOTALL)
        if match:
            directive = match.group(1)
            args = match.group(2).strip()
            if directive in supported:
                steps.append(BuildStep(directive=directive, args=args))

    return steps


def _raise_for_status(response: httpx.Response) -> None:
    """Raise an appropriate MVMError for non-success HTTP responses."""
    if response.status_code >= 200 and response.status_code < 300:
        return

    # Try to extract error message from JSON body.
    message = ""
    try:
        body = response.json()
        message = body.get("error", "")
    except Exception:
        message = response.text or "unknown error"

    code = response.status_code
    if code == 401:
        raise AuthError(message, status_code=code)
    elif code == 404:
        raise NotFoundError(message, status_code=code)
    elif code == 409:
        raise ConflictError(message, status_code=code)
    elif code >= 500:
        raise ServerError(message, status_code=code)
    else:
        raise MVMError(message, status_code=code)


class VM:
    """A handle to a running VM, returned by Sandbox.create()."""

    def __init__(self, client: "Sandbox", name: str, info: VMInfo) -> None:
        self._client = client
        self.name = name
        self.info = info

    def exec(self, command: str, *, stdin: str = "") -> ExecResult:
        """Execute a command inside the VM."""
        payload = {"command": command}
        if stdin:
            payload["stdin"] = stdin
        resp = self._client._request("POST", "/vms/{name}/exec".format(name=self.name), json=payload)
        data = resp.json()
        return ExecResult(output=data.get("output", ""), exit_code=data.get("exit_code", 0))

    def stop(self, *, force: bool = False) -> None:
        """Stop the VM."""
        payload = {}
        if force:
            payload["force"] = True
        self._client._request("POST", "/vms/{name}/stop".format(name=self.name), json=payload)

    def delete(self, *, force: bool = False) -> None:
        """Delete the VM. If force is True, stop it first."""
        if force:
            try:
                self.stop(force=True)
            except MVMError:
                pass
        self._client._request("DELETE", "/vms/{name}".format(name=self.name))

    def pause(self) -> None:
        """Pause the VM."""
        self._client._request("POST", "/vms/{name}/pause".format(name=self.name))

    def resume(self) -> None:
        """Resume a paused VM."""
        self._client._request("POST", "/vms/{name}/resume".format(name=self.name))

    def snapshot(self, name: str) -> None:
        """Create a snapshot of the VM."""
        self._client._request(
            "POST", "/vms/{name}/snapshot".format(name=self.name), json={"name": name}
        )

    def restore(self, snapshot_name: str) -> None:
        """Restore the VM from a snapshot."""
        self._client._request(
            "POST", "/vms/{name}/restore".format(name=self.name), json={"name": snapshot_name}
        )

    def __repr__(self) -> str:
        return "VM(name={name!r}, status={status!r})".format(name=self.name, status=self.info.status)


class Sandbox:
    """Client for the mvm sandbox daemon.

    Use Sandbox.connect() to create an instance.
    """

    def __init__(self, http_client: httpx.Client, base_url: str) -> None:
        self._http = http_client
        self._base_url = base_url

    @classmethod
    def connect(
        cls,
        url: str = "",
        *,
        api_key: str = "",
        ca_cert: Optional[str] = None,
    ) -> "Sandbox":
        """Connect to a local or remote mvm daemon.

        Args:
            url: Remote URL (e.g. "https://host:19876"). If empty, connects
                 via local Unix socket.
            api_key: Bearer token for authentication (required for remote).
            ca_cert: Path to a CA certificate file for TLS verification.

        Returns:
            A connected Sandbox client.
        """
        headers = {}
        if api_key:
            headers["Authorization"] = "Bearer " + api_key

        if url:
            # Remote mode: standard HTTPS/HTTP client.
            verify = ca_cert if ca_cert else True
            client = httpx.Client(
                base_url=url,
                headers=headers,
                verify=verify,
                timeout=300.0,
            )
            return cls(client, url)
        else:
            # Local mode: Unix socket transport.
            sock_path = _default_socket_path()
            transport = httpx.HTTPTransport(uds=sock_path)
            client = httpx.Client(
                transport=transport,
                base_url="http://localhost",
                headers=headers,
                timeout=300.0,
            )
            return cls(client, "http://localhost")

    def _request(
        self,
        method: str,
        path: str,
        *,
        json: Optional[dict] = None,
    ) -> httpx.Response:
        """Send an HTTP request and raise on error."""
        resp = self._http.request(method, path, json=json)
        _raise_for_status(resp)
        return resp

    def create(
        self,
        name: str,
        *,
        cpus: int = 0,
        memory_mb: int = 0,
        image: str = "",
        net_policy: str = "",
    ) -> VM:
        """Create a new sandbox VM.

        Args:
            name: VM name (alphanumeric + hyphens).
            cpus: Number of vCPUs (0 = default).
            memory_mb: Memory in MiB (0 = default).
            image: Custom rootfs image name (empty = base image).
            net_policy: Network policy ("allow-all", "block-all", etc.).

        Returns:
            A VM handle.
        """
        payload: dict = {"name": name}
        if cpus > 0:
            payload["cpus"] = cpus
        if memory_mb > 0:
            payload["memory_mb"] = memory_mb
        if image:
            payload["image"] = image
        if net_policy:
            payload["net_policy"] = net_policy

        resp = self._request("POST", "/vms", json=payload)
        data = resp.json()
        info = VMInfo(
            name=data.get("name", name),
            status=data.get("status", "running"),
            guest_ip=data.get("guest_ip", ""),
            pid=data.get("pid", 0),
            created_at=data.get("created_at", ""),
        )
        return VM(self, name, info)

    def list(self) -> List[VMInfo]:
        """List all VMs."""
        resp = self._request("GET", "/vms")
        data = resp.json()
        result: List[VMInfo] = []
        for item in data:
            result.append(
                VMInfo(
                    name=item.get("name", ""),
                    status=item.get("status", ""),
                    guest_ip=item.get("guest_ip", ""),
                    pid=item.get("pid", 0),
                    created_at=item.get("created_at", ""),
                )
            )
        return result

    def build(self, dockerfile: str, *, tag: str, size_mb: int = 512) -> None:
        """Build a custom rootfs image from a Dockerfile string.

        Args:
            dockerfile: Dockerfile content (supports RUN, ENV, COPY, ADD).
            tag: Image name/tag for the built image.
            size_mb: Rootfs size in MiB (default 512).
        """
        steps = _parse_dockerfile(dockerfile)
        payload = {
            "image_name": tag,
            "steps": [{"directive": s.directive, "args": s.args} for s in steps],
            "size_mb": size_mb,
        }
        self._request("POST", "/build", json=payload)

    def images(self) -> List[ImageInfo]:
        """List custom rootfs images."""
        resp = self._request("GET", "/images")
        data = resp.json()
        return [ImageInfo(name=item.get("name", ""), size_mb=item.get("size_mb", 0)) for item in data]

    def delete_image(self, name: str) -> None:
        """Delete a custom rootfs image."""
        self._request("DELETE", "/images/{name}".format(name=name))

    def snapshots(self) -> List[SnapshotInfo]:
        """List all snapshots."""
        resp = self._request("GET", "/snapshots")
        data = resp.json()
        return [
            SnapshotInfo(
                name=item.get("name", ""),
                vm=item.get("vm", ""),
                created=item.get("created", ""),
                type=item.get("type", ""),
            )
            for item in data
        ]

    def delete_snapshot(self, name: str) -> None:
        """Delete a snapshot."""
        self._request("DELETE", "/snapshots/{name}".format(name=name))

    def pool_status(self) -> PoolStatus:
        """Get the VM pool status."""
        resp = self._request("GET", "/pool")
        data = resp.json()
        return PoolStatus(ready=data.get("ready", 0), total=data.get("total", 0))

    def pool_warm(self) -> None:
        """Trigger pool warming."""
        self._request("POST", "/pool/warm")

    def close(self) -> None:
        """Close the underlying HTTP client."""
        self._http.close()

    def __enter__(self) -> "Sandbox":
        return self

    def __exit__(self, *args: object) -> None:
        self.close()

    def __repr__(self) -> str:
        return "Sandbox(url={url!r})".format(url=self._base_url)
