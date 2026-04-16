# mvm Python SDK

Python SDK for [mvm](https://github.com/paulmeller/mvm) — local and cloud microVM sandboxes for AI agents.

## Install

```bash
pip install mvm-sandbox
```

## Usage

```python
from mvm_sandbox import Sandbox

# Connect to remote daemon
client = Sandbox.connect("https://server:19876", api_key="secret")

# Or local (auto-detects Unix socket)
client = Sandbox.connect()

# Create a sandbox
vm = client.create("my-sandbox", cpus=2, memory_mb=512)

# Execute commands
result = vm.exec("echo hello")
print(result.output)     # "hello\n"
print(result.exit_code)  # 0

# Checkpoint and restore
vm.snapshot("before-install")
vm.exec("pip install pandas")
vm.restore("before-install")

# Cleanup
vm.delete()
```

## Requirements

- Python 3.9+
- A running mvm daemon (local or remote)
