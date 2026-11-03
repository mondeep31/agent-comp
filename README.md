# agent-comp

Computers for AI agents, in a snap. Each agent gets its own Firecracker microVM: a full Linux machine with hardware-level isolation, created from a memory snapshot in tens of milliseconds.

- `acd`: Go daemon. Runs the microVMs, the warm pool, networking, and the REST/WebSocket and MCP APIs.
- `acctl`: Go CLI for the API.
- `agentd`: Rust guest agent (PID 1 in every microVM). Serves exec and file operations over vsock.

Plan: [`docs/PLAN.md`](docs/PLAN.md). Measured numbers: [`docs/BENCHMARKS.md`](docs/BENCHMARKS.md). Agents working on this repo: read [`AGENTS.md`](AGENTS.md).

## Development

MicroVMs need Linux with KVM. Development happens on a Mac, and everything that touches KVM runs on the dev host over Tailscale (`ssh comp-agent`).

```bash
make help              # list targets
make remote-lint       # sync the tree to the dev host and lint Go, Rust and protobuf there
make remote-test       # unit tests on the dev host
make remote-build      # build acd, acctl and agentd on the dev host
make remote-integration  # KVM integration tests on the dev host
```

Host prerequisites (installed on `comp-agents`): Firecracker and jailer, Go, Rust with the `x86_64-unknown-linux-musl` target, `buf`, `protoc-gen-go`, `golangci-lint`, `protobuf-compiler`, `musl-tools`.

Regenerate protobuf code with `make remote-proto` and copy `gen/` back, or run `make proto` anywhere `buf` and `protoc-gen-go` are installed.
