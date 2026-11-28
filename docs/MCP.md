# MCP server

agent-comp exposes sandboxes to agents as MCP tools. Each "computer" is a
Firecracker microVM.

| Tool | What it does |
| --- | --- |
| `list_images` | Images with their default size, and which have a screen |
| `create_computer` | Start a computer (optionally `image` or `from_snapshot`), returns its id |
| `list_computers` | Your computers with state and expiry |
| `run_command` | Shell command; exit code, stdout, stderr (1 MiB each) |
| `read_file` | Read up to 1 MiB; text as UTF-8, binary as base64 |
| `write_file` | Create or replace a file (`encoding: base64` for binary) |
| `list_files` | Directory listing |
| `snapshot_computer` | Save memory and disk; fork with `create_computer` |
| `destroy_computer` | Delete a computer |
| `computer_action` | Use the screen of a computer with a display: `screenshot`, clicks, `type`, `key`, `scroll`, ... |

`computer_action` takes `computer_id`, `action` and the action's parameters
(see [Screen in the API](API.md#screen)). It returns the cursor position and,
for every action except `cursor_position` and `wait`, a screenshot as MCP
image content (`"screenshot": false` turns it off). Create the computer with
`image: "desktop"` to get a screen; it boots into a maximized Chromium.

There are two transports and both use the same API key.

## Streamable HTTP (remote agents)

The endpoint is `https://<your-domain>/v1/mcp` with
`Authorization: Bearer <key>`. Example for Cursor (`.cursor/mcp.json`):

```json
{
  "mcpServers": {
    "agent-comp": {
      "url": "https://agents.example.com/v1/mcp",
      "headers": { "Authorization": "Bearer ${env:AC_API_KEY}" }
    }
  }
}
```

The server pings each session every 25 s. Cloudflare closes HTTP responses
that stay silent for 100 s, so `run_command` sends a progress notification
every 15 s when the client supplies a progress token. Without one, keep
commands under 100 s and run longer jobs in the background
(`nohup make > build.log 2>&1 &`), then poll.

## stdio (local agents)

`acctl mcp` serves the same tools over stdio and forwards them to the API.
Commands run over the WebSocket API, so long commands are not cut off.

```json
{
  "mcpServers": {
    "agent-comp": {
      "command": "acctl",
      "args": ["mcp"],
      "env": { "AC_URL": "https://agents.example.com", "AC_API_KEY": "ac_…" }
    }
  }
}
```

## Security notes

- Every MCP request is authenticated like the REST API. The tools act as the
  key's owner and cannot see other keys' computers.
- The SDK's DNS-rebinding guard is disabled because cloudflared delivers
  requests over loopback with the public `Host` header. The bearer key
  requirement already protects against rebinding.
