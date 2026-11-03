# Computers for AI agents.

Give every agent its own Linux machine. agent-comp creates isolated Firecracker microVMs from memory snapshots, with shell execution, file access, and outbound networking through a single API.

## From idea to execution

Your agent asks for a computer. The control plane takes a prepared VM from the warm pool, configures its identity, and returns a sandbox. Commands run inside a dedicated guest kernel; files stay in that sandbox's private writable overlay.

- **Start building:** [Create your first sandbox](quickstart.html).
- **Connect an agent:** [Configure MCP](mcp.html) over HTTP or stdio.
- **Integrate directly:** [Explore the HTTP and WebSocket API](api.html).
- **Run the platform:** [Deploy on a Linux KVM host](deploy.html).

## One computer, three interfaces

| Interface | Best for |
| --- | --- |
| acctl CLI | Terminal workflows, scripts, and local MCP |
| HTTP + WebSocket | Applications, file transfer, and streaming commands |
| MCP tools | Agents that create computers, run commands, and manage files |

## Built for a fresh start

The Go control plane manages lifecycle, authentication, quotas, snapshots, and networking. A small Rust agent inside each VM handles commands and files over vsock. Firecracker and its jailer provide the VM boundary.

The warm-pool benchmark reached **9.7 ms p50** to a configured guest on the development host. This is a pool measurement, not an internet API latency promise. Deployed warm creates measured 37–44 ms on the server; a quick-tunnel request measured 578 ms round trip. See [benchmarks and methodology](benchmarks.html).

## Project status

The core v1 implementation is complete and running on a development host. This is a single-host, headless Linux platform. Production readiness, durable user snapshots, GUI desktops, and a multi-host scheduler are not implied by that milestone.

The next priorities are a stable API hostname, individual agent credentials, and the remaining security hardening. [Read the status and next steps](roadmap.html).
