# AGENTS.md

Guidance for any AI agent working in this repository.

## Shared memory: innernet

Decisions, host facts, benchmarks, task status and handoffs live in the innernet project **`agent-comp`** (MCP namespace `user-innernet`). The repo holds code and docs; innernet holds state.

1. Session start: `innernet_handoff_read` for `agent-comp`, then `innernet_context` with the current ask.
2. During work: `innernet_capture` gotchas and decisions as they happen.
3. After each stage is pushed: `innernet_save_context` for changed dimensions, then `innernet_handoff_write` (what shipped, commit SHA, next steps, open issues).

Never store secrets in innernet or in the repo: API keys, Cloudflare tunnel tokens, SSH private keys, `.env` contents.

## Project

Firecracker microVM "computers" for AI agents. Go control plane (`acd`, `acctl`) plus a Rust guest agent (`agentd`) over vsock. See `docs/PLAN.md` for the plan and `docs/BENCHMARKS.md` for measured numbers.

## Dev host

- VMs only run on the Linux host, reached with `ssh comp-agent` (Tailscale). This Mac has no KVM.
- `comp-agents` is development only and runs Firecracker under nested virtualization. Tag every benchmark with its host.

## Rules

- Run `make lint test` before every commit. Commit with Conventional Commits and push to `main` after each stage.
- Go: `context.Context` first on blocking paths, wrap errors with `%w`, `log/slog`, no globals beyond `main`, table-driven tests with `-race`. KVM tests use the `integration` build tag.
- Rust: no `unwrap`/`expect` outside tests, `thiserror` in libraries and `anyhow` only at the binary edge, clippy with `-D warnings`.
- Sandbox egress must never reach the tailnet (`100.64.0.0/10`), the host subnet, RFC1918 ranges or `169.254.169.254`.
