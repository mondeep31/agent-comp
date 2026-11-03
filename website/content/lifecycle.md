# Lifecycle & snapshots

A sandbox is an owned, time-limited computer. Its API-visible states are running, paused, and stopped.

## Create and use

Creation chooses a base image and resource shape, then uses the warm pool or restores a golden snapshot. A custom shape may need a new golden snapshot. The create response means the guest has been configured and is ready for operations.

Defaults in the example configuration are one vCPU, 256 MiB RAM, 2 GiB disk, a one-hour TTL, and a 15-minute idle timeout. Operators can change these values.

## Pause, resume, and expiry

- Idle timeout pauses a sandbox without discarding its memory or disk.
- An operation on a paused sandbox resumes it transparently.
- Explicit pause and resume endpoints are available.
- At expiry, the sandbox stops. Stopped records remain visible for 10 minutes with a stop reason.
- Deleting a sandbox releases its resources.

## Fork a snapshot

`POST /v1/sandboxes/{id}/snapshot` captures memory and disk. Create a new sandbox with `from_snapshot` set to that snapshot's ID to fork its execution state.

```json
{"from_snapshot": "snapshot-id-from-the-api"}
```

The fork gets its own hostname, clock, and entropy configuration. Snapshot ownership follows the API key and is protected by the same isolation rules as sandbox ownership.

## Restart behavior

**Restarting acd destroys running sandboxes and user snapshots.** Export important files before a restart. Golden image snapshots are cached on disk and can be reused after their fingerprint is checked; that cache does not preserve user work.

## Long-running commands

Use WebSocket exec for long commands, particularly through the tunnel. Closing the socket kills the command. For MCP over HTTP, progress notifications keep long requests active only when the client supplies a progress token. The stdio MCP bridge uses WebSocket execution.
