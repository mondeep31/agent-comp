# Development

The control plane is Go; the guest agent is Rust. Protobuf defines their shared protocol. All routine checks run through the repository Makefile.

## Build environment

VM execution requires Linux x86_64 with KVM and cgroup v2. macOS can edit source and build the CLI, but cannot run these microVM integration tests. Install Go, Rust with the x86_64-unknown-linux-musl target, Firecracker and jailer, buf, protoc-gen-go, golangci-lint, protobuf-compiler, and musl-tools on the development host.

```sh
make help
make lint test
make build
make proto
make integration
make audit
```

`make integration` requires KVM and elevated privileges. The configured development workflow supports `make remote-lint`, `make remote-test`, `make remote-build`, and `make remote-integration` via the SSH host alias in the Makefile.

## Code conventions

- Go: context first on blocking paths, errors wrapped with `%w`, structured slog logging, table-driven tests with the race detector.
- Rust: no unwrap or expect outside tests; thiserror in libraries, anyhow at the binary edge; clippy warnings fail the build.
- KVM tests use the integration build tag.
- Run `make lint test` before every commit. Use Conventional Commits and push completed stages to main.

## CI and releases

GitHub-hosted CI runs language checks, builds, protobuf checks, and dependency audits. A dedicated self-hosted KVM runner executes integration tests on trusted main commits and manual dispatches. Version tags trigger release artifacts and checksums.

The source repository remains private because its integration runner has privileged host access. Documentation publishing must not require changing repository visibility.

## Shared project memory

innernet project `agent-comp` holds decisions, host facts, benchmarks, status, and handoffs. Read the handoff and task context at session start; capture decisions during work; save changed context and a handoff after each pushed stage. Never store credentials or environment-file contents.

The original PLAN.md is a historical implementation plan, not a live completion checklist. Use [Status & next steps](roadmap.html) for the current state.

## Documentation site

The website uses Python's standard library to render curated guides and the repository's API, MCP, deployment, security, and benchmark Markdown. No package installation is needed.

```sh
python3 website/build.py
python3 -m http.server 4173 --directory website/dist
```

Edit the original docs for references or `website/content` for guides, then rebuild. Publish `website/dist` to static hosting.
