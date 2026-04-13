# Contributing to mvm

Thanks for your interest in contributing to mvm!

## Getting Started

1. Fork the repo
2. Clone your fork: `git clone https://github.com/<you>/mvm.git`
3. Create a branch: `git checkout -b my-feature`
4. Make your changes
5. Build and test: `make build && ./bin/mvm version`
6. Commit and push
7. Open a pull request

## Development Setup

Requirements:
- Go 1.26+
- Apple Silicon Mac (M3+) with macOS 15+ (for running VMs)
- Lima (`brew install lima`)

```bash
make build    # build binary to ./bin/mvm
make test     # run tests
make install  # install to $GOPATH/bin
```

## Architecture

```
cmd/mvm/main.go           Entry point
internal/cli/              Cobra commands
internal/lima/             Lima VM management (limactl wrapper)
internal/firecracker/      Firecracker install, config, process, pool
internal/state/            State persistence, networking
```

All Firecracker interactions happen inside a Lima VM via `limactl shell`. The CLI on macOS orchestrates everything through this bridge.

## Guidelines

- Keep PRs focused on a single change
- Test your changes end-to-end (`mvm init`, `mvm start`, `mvm ssh`, `mvm stop`, `mvm delete`)
- Run `go vet ./...` before submitting
- Follow existing code patterns

## Reporting Issues

Please include:
- macOS version and chip (`sw_vers` and `sysctl machdep.cpu.brand_string`)
- mvm version (`mvm version`)
- Steps to reproduce
- Relevant logs (`mvm logs <name> --boot`)
