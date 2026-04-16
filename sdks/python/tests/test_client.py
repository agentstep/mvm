"""Tests for the mvm sandbox Python SDK client."""

import json

import httpx
import pytest
from pytest_httpx import HTTPXMock

from mvm_sandbox import (
    AuthError,
    ExecResult,
    NotFoundError,
    Sandbox,
    VMInfo,
)
from mvm_sandbox.client import VM, _parse_dockerfile


# --- Helpers ---


def _make_sandbox(httpx_mock: HTTPXMock, api_key: str = "") -> Sandbox:
    """Create a Sandbox client that uses httpx mock transport."""
    headers = {}
    if api_key:
        headers["Authorization"] = "Bearer " + api_key
    client = httpx.Client(base_url="http://testserver", headers=headers)
    return Sandbox(client, "http://testserver")


# --- Sandbox.connect() ---


def test_connect_with_url():
    """connect() with a URL creates a remote client."""
    sb = Sandbox.connect("https://example.com:19876", api_key="secret")
    assert sb._base_url == "https://example.com:19876"
    sb.close()


def test_connect_sets_auth_header():
    """connect() with api_key sets the Authorization header."""
    sb = Sandbox.connect("http://localhost:19876", api_key="test-key")
    auth = sb._http.headers.get("authorization")
    assert auth == "Bearer test-key"
    sb.close()


# --- Sandbox.create() ---


def test_create_returns_vm(httpx_mock: HTTPXMock):
    """create() returns a VM object with correct name and status."""
    httpx_mock.add_response(
        method="POST",
        url="http://testserver/vms",
        json={
            "name": "myvm",
            "status": "running",
            "guest_ip": "10.0.0.2",
            "pid": 1234,
            "created_at": "2025-01-01T00:00:00Z",
        },
        status_code=201,
    )
    sb = _make_sandbox(httpx_mock)
    vm = sb.create("myvm")

    assert isinstance(vm, VM)
    assert vm.name == "myvm"
    assert vm.info.status == "running"
    assert vm.info.guest_ip == "10.0.0.2"
    assert vm.info.pid == 1234

    # Verify request body.
    request = httpx_mock.get_request()
    assert request is not None
    body = json.loads(request.content)
    assert body["name"] == "myvm"


def test_create_with_options(httpx_mock: HTTPXMock):
    """create() sends optional parameters in the request."""
    httpx_mock.add_response(
        method="POST",
        url="http://testserver/vms",
        json={"name": "dev", "status": "running"},
        status_code=201,
    )
    sb = _make_sandbox(httpx_mock)
    sb.create("dev", cpus=2, memory_mb=512, image="python", net_policy="block-all")

    request = httpx_mock.get_request()
    assert request is not None
    body = json.loads(request.content)
    assert body["cpus"] == 2
    assert body["memory_mb"] == 512
    assert body["image"] == "python"
    assert body["net_policy"] == "block-all"


# --- VM.exec() ---


def test_exec_returns_result(httpx_mock: HTTPXMock):
    """exec() returns an ExecResult with output and exit code."""
    httpx_mock.add_response(
        method="POST",
        url="http://testserver/vms/myvm/exec",
        json={"output": "hello world\n", "exit_code": 0},
    )
    sb = _make_sandbox(httpx_mock)
    info = VMInfo(name="myvm", status="running")
    vm = VM(sb, "myvm", info)
    result = vm.exec("echo hello world")

    assert isinstance(result, ExecResult)
    assert result.output == "hello world\n"
    assert result.exit_code == 0

    request = httpx_mock.get_request()
    assert request is not None
    body = json.loads(request.content)
    assert body["command"] == "echo hello world"


def test_exec_with_stdin(httpx_mock: HTTPXMock):
    """exec() sends stdin when provided."""
    httpx_mock.add_response(
        method="POST",
        url="http://testserver/vms/myvm/exec",
        json={"output": "got it", "exit_code": 0},
    )
    sb = _make_sandbox(httpx_mock)
    info = VMInfo(name="myvm", status="running")
    vm = VM(sb, "myvm", info)
    vm.exec("cat", stdin="input data")

    request = httpx_mock.get_request()
    assert request is not None
    body = json.loads(request.content)
    assert body["stdin"] == "input data"


def test_exec_nonzero_exit(httpx_mock: HTTPXMock):
    """exec() captures nonzero exit codes."""
    httpx_mock.add_response(
        method="POST",
        url="http://testserver/vms/myvm/exec",
        json={"output": "not found", "exit_code": 1},
    )
    sb = _make_sandbox(httpx_mock)
    info = VMInfo(name="myvm", status="running")
    vm = VM(sb, "myvm", info)
    result = vm.exec("false")

    assert result.exit_code == 1


# --- Sandbox.list() ---


def test_list_returns_vms(httpx_mock: HTTPXMock):
    """list() returns a list of VMInfo objects."""
    httpx_mock.add_response(
        method="GET",
        url="http://testserver/vms",
        json=[
            {"name": "vm1", "status": "running", "guest_ip": "10.0.0.2", "pid": 100},
            {"name": "vm2", "status": "stopped", "guest_ip": "", "pid": 0},
        ],
    )
    sb = _make_sandbox(httpx_mock)
    vms = sb.list()

    assert len(vms) == 2
    assert all(isinstance(v, VMInfo) for v in vms)
    assert vms[0].name == "vm1"
    assert vms[0].status == "running"
    assert vms[1].name == "vm2"
    assert vms[1].status == "stopped"


def test_list_empty(httpx_mock: HTTPXMock):
    """list() returns empty list when no VMs exist."""
    httpx_mock.add_response(method="GET", url="http://testserver/vms", json=[])
    sb = _make_sandbox(httpx_mock)
    vms = sb.list()
    assert vms == []


# --- Auth header ---


def test_auth_header_sent(httpx_mock: HTTPXMock):
    """API key is sent as Bearer token in Authorization header."""
    httpx_mock.add_response(method="GET", url="http://testserver/vms", json=[])
    sb = _make_sandbox(httpx_mock, api_key="my-secret-key")
    sb.list()

    request = httpx_mock.get_request()
    assert request is not None
    assert request.headers.get("authorization") == "Bearer my-secret-key"


def test_no_auth_header_without_key(httpx_mock: HTTPXMock):
    """No Authorization header when api_key is not set."""
    httpx_mock.add_response(method="GET", url="http://testserver/vms", json=[])
    sb = _make_sandbox(httpx_mock)
    sb.list()

    request = httpx_mock.get_request()
    assert request is not None
    assert "authorization" not in request.headers


# --- Error handling ---


def test_401_raises_auth_error(httpx_mock: HTTPXMock):
    """401 response raises AuthError."""
    httpx_mock.add_response(
        method="GET",
        url="http://testserver/vms",
        json={"error": "unauthorized"},
        status_code=401,
    )
    sb = _make_sandbox(httpx_mock)
    with pytest.raises(AuthError) as exc_info:
        sb.list()
    assert exc_info.value.status_code == 401
    assert "unauthorized" in str(exc_info.value)


def test_404_raises_not_found(httpx_mock: HTTPXMock):
    """404 response raises NotFoundError."""
    httpx_mock.add_response(
        method="DELETE",
        url="http://testserver/vms/noexist",
        json={"error": "not found"},
        status_code=404,
    )
    sb = _make_sandbox(httpx_mock)
    info = VMInfo(name="noexist", status="running")
    vm = VM(sb, "noexist", info)
    with pytest.raises(NotFoundError) as exc_info:
        vm.delete()
    assert exc_info.value.status_code == 404


# --- Dockerfile parsing ---


def test_parse_dockerfile():
    """_parse_dockerfile extracts supported directives."""
    dockerfile = """
FROM ubuntu:22.04
RUN apt-get update
RUN apt-get install -y python3
ENV MY_VAR=hello
COPY myfile /app/myfile
WORKDIR /app
"""
    steps = _parse_dockerfile(dockerfile)
    assert len(steps) == 4
    assert steps[0].directive == "RUN"
    assert steps[0].args == "apt-get update"
    assert steps[1].directive == "RUN"
    assert steps[2].directive == "ENV"
    assert steps[2].args == "MY_VAR=hello"
    assert steps[3].directive == "COPY"


def test_parse_dockerfile_continuation():
    """_parse_dockerfile handles backslash continuation lines."""
    dockerfile = """RUN apt-get update && \\
    apt-get install -y python3
"""
    steps = _parse_dockerfile(dockerfile)
    assert len(steps) == 1
    assert "apt-get update" in steps[0].args
    assert "apt-get install" in steps[0].args


# --- Context manager ---


def test_context_manager(httpx_mock: HTTPXMock):
    """Sandbox works as a context manager."""
    httpx_mock.add_response(method="GET", url="http://testserver/vms", json=[])
    with _make_sandbox(httpx_mock) as sb:
        sb.list()
    # After exiting context, client should be closed.
    # We just verify no exception was raised.


# --- VM operations ---


def test_vm_stop(httpx_mock: HTTPXMock):
    """VM.stop() sends POST to /vms/{name}/stop."""
    httpx_mock.add_response(
        method="POST",
        url="http://testserver/vms/myvm/stop",
        status_code=204,
    )
    sb = _make_sandbox(httpx_mock)
    vm = VM(sb, "myvm", VMInfo(name="myvm", status="running"))
    vm.stop()

    request = httpx_mock.get_request()
    assert request is not None
    assert request.method == "POST"


def test_vm_stop_force(httpx_mock: HTTPXMock):
    """VM.stop(force=True) sends force flag in body."""
    httpx_mock.add_response(
        method="POST",
        url="http://testserver/vms/myvm/stop",
        status_code=204,
    )
    sb = _make_sandbox(httpx_mock)
    vm = VM(sb, "myvm", VMInfo(name="myvm", status="running"))
    vm.stop(force=True)

    request = httpx_mock.get_request()
    assert request is not None
    body = json.loads(request.content)
    assert body["force"] is True


def test_vm_pause_resume(httpx_mock: HTTPXMock):
    """VM.pause() and VM.resume() send correct requests."""
    httpx_mock.add_response(
        method="POST",
        url="http://testserver/vms/myvm/pause",
        status_code=204,
    )
    httpx_mock.add_response(
        method="POST",
        url="http://testserver/vms/myvm/resume",
        status_code=204,
    )
    sb = _make_sandbox(httpx_mock)
    vm = VM(sb, "myvm", VMInfo(name="myvm", status="running"))
    vm.pause()
    vm.resume()

    requests = httpx_mock.get_requests()
    assert len(requests) == 2
    assert requests[0].url.path == "/vms/myvm/pause"
    assert requests[1].url.path == "/vms/myvm/resume"


def test_vm_repr():
    """VM.__repr__ returns a useful string."""
    sb = Sandbox.__new__(Sandbox)
    vm = VM(sb, "test", VMInfo(name="test", status="running"))
    assert "test" in repr(vm)
    assert "running" in repr(vm)
