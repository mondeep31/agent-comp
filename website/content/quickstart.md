# Your first computer

Use an existing acd service and the acctl CLI to create a sandbox, execute code, and clean up. Operators setting up the service for the first time should follow [Deployment](deploy.html).

## Before you start

You need a reachable API endpoint, a bearer key issued by its operator, and acctl built from this repository. The server needs Linux with KVM; the client does not boot VMs locally.

```sh
# Build the CLI for your local OS and architecture.
go build -o acctl ./cmd/acctl
export AC_URL=https://computers.example.com
# Set AC_API_KEY through your secret manager or a private shell session.
```

The example hostname is a placeholder. A named production endpoint has not yet been provisioned for this project.

## Create and run

```sh
id=$(./acctl create -q)
./acctl sh "$id" 'echo hello from $(hostname)'
echo 'print(2+2)' | ./acctl put "$id" - /work/main.py
./acctl exec "$id" -- python3 /work/main.py
```

Each API key owns its own resources. Other keys cannot enumerate or access your sandbox.

## Clean up

```sh
./acctl rm "$id"
```

Sandboxes also stop when their TTL expires. Idle sandboxes pause and resume automatically when you next use them. Neither mechanism makes files durable across a daemon restart.

## Choose your next path

- [MCP integration](mcp.html): expose computers as tools to your agent.
- [API reference](api.html): create, execute, stream, transfer files, and fork snapshots.
- [Lifecycle](lifecycle.html): understand pause, expiry, snapshots, and restart behavior.
