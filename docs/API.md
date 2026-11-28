# agent-comp HTTP API (v1)

Every `/v1` request needs `Authorization: Bearer <key>`. Keys come from
`acctl keygen`; the key's name owns the sandboxes and snapshots it creates, and
no key can see another key's resources (they get `404`). Each key is
rate-limited (`limits.requests_per_second` / `request_burst`).

`GET /healthz` is unauthenticated.

## Errors

Non-2xx responses carry:

```json
{"error": {"code": "not_found", "message": "sandbox sb-…: not found"}}
```

| Status | Code | Meaning |
| --- | --- | --- |
| 400 | `invalid_argument` | Bad request body or parameter |
| 401 | `unauthenticated` | Missing or unknown key |
| 403 | `permission_denied` | The guest refused a file operation |
| 404 | `not_found` | No such sandbox, snapshot, file or program |
| 409 | `conflict` | Wrong state, e.g. the sandbox is stopped |
| 413 | `too_large` | File over `limits.max_file_bytes` |
| 429 | `quota_exceeded` / `rate_limited` | Sandbox, snapshot or host memory quota, or request rate |
| 503 | `unavailable` | acd is shutting down, or a sandbox's display is not ready |
| 500 | `internal` | Details are in acd's log, never in the response |

## Images

`GET /v1/images` lists the images sandboxes can start from:

```json
{"images": [
  {"name": "base", "default": true, "vcpu": 1, "memory_mib": 256, "disk_mib": 2048},
  {"name": "desktop", "default": false, "vcpu": 1, "memory_mib": 1024, "disk_mib": 2048,
   "display": {"width": 1280, "height": 800}}
]}
```

Images with a `display` have a screen (see [Screen](#screen)). The shape is
each image's default for new sandboxes.

## Sandboxes

| Method and path | Body | Response |
| --- | --- | --- |
| `POST /v1/sandboxes` | `CreateSandboxRequest` (optional) | `201` Sandbox |
| `GET /v1/sandboxes` | | `{"sandboxes": [Sandbox]}` |
| `GET /v1/sandboxes/{id}` | | Sandbox |
| `DELETE /v1/sandboxes/{id}` | | `204` |
| `POST /v1/sandboxes/{id}/pause` | | Sandbox |
| `POST /v1/sandboxes/{id}/resume` | | Sandbox |
| `POST /v1/sandboxes/{id}/snapshot` | | `201` Snapshot |

`CreateSandboxRequest` fields are all optional: `image` (default: the first
configured image; shape fields default to the image's shape), `vcpu`, `memory_mib`,
`disk_mib`, `ttl_seconds`, `idle_timeout_seconds`, `env` (default environment
for every command), `from_snapshot` (fork a snapshot instead of a fresh
image) and `metadata` (up to 16 string pairs).

A sandbox is `running`, `paused` or `stopped`. It pauses after
`idle_timeout` without operations, and any operation on a paused sandbox
resumes it transparently. At `expires_at` it stops; stopped sandboxes are
listed (with `stop_reason`) for 10 minutes.

## Commands

`POST /v1/sandboxes/{id}/exec` runs a command to completion:

```json
{"command": "pip install requests && python -c 'import requests'", "timeout_seconds": 120}
```

Use `argv` (`["python3", "-V"]`) instead of `command` to skip the shell. Other
fields: `env`, `cwd`, `stdin` (string), `max_output_bytes` (each stream,
up to 1 MiB). The response:

```json
{"exit_code": 0, "timed_out": false, "duration_ms": 812,
 "stdout": "…", "stderr": "", "stdout_truncated": false}
```

`exit_code` is `-1` when the process was killed; `signal` then holds the
signal number. Through the Cloudflare tunnel an HTTP response must start
within 100 s, so use the WebSocket endpoint for anything longer.

### Streaming: `GET /v1/sandboxes/{id}/exec/ws`

A WebSocket carrying JSON messages, each with a `type`. Byte payloads (`data`)
are base64.

1. The client sends `start` with the same fields as the exec body (except
   `stdin` and `max_output_bytes`).
2. The server answers `started`, then streams `stdout` / `stderr`.
3. The client may send `stdin` (`data`), `stdin_eof` and `signal`
   (`signal`: number) at any time.
4. The server ends with one `exit` (`exit_code`, `signal`, `timed_out`,
   `duration_ms`) or `error` (`error.code`, `error.message`) and closes.

Closing the socket kills the command. The server pings every 20 s.

## Files

All paths are absolute paths inside the sandbox.

| Method and path | Body | Response |
| --- | --- | --- |
| `GET /v1/sandboxes/{id}/files?path=` | | Raw bytes |
| `PUT /v1/sandboxes/{id}/files?path=[&mode=0644][&append=true]` | Raw bytes | File |
| `GET /v1/sandboxes/{id}/ls?path=` | | `{"path", "entries": [File]}` |
| `GET /v1/sandboxes/{id}/stat?path=` | | File |

Writes create parent directories. A File is `{path, name, type, size, mode,
mtime}` with `type` one of `file`, `dir`, `symlink`, `other`.

## Screen

Sandboxes from an image with a `display` (such as `desktop`) run a virtual
screen with a browser. Their Sandbox object has `display: {width, height}`.
On other sandboxes these endpoints return `400`, and while the screen is
still starting (or restarting after a crash) they return `503`.

| Method and path | Body | Response |
| --- | --- | --- |
| `GET /v1/sandboxes/{id}/screenshot[?format=png\|jpeg][&quality=1-100][&region=x0,y0,x1,y1]` | | Image bytes |
| `POST /v1/sandboxes/{id}/computer` | Action | `{"cursor_x", "cursor_y", "image"?}` |

A screenshot is `image/png` (default) or `image/jpeg`, with
`X-Screen-Width` and `X-Screen-Height` headers. JPEG is about half the
latency of PNG for the same screen.

An Action uses the computer-use vocabulary agents already speak, so a
model's tool call can be forwarded as is:

| `action` | Parameters |
| --- | --- |
| `screenshot` | |
| `zoom` | `region` `[x0, y0, x1, y1]`: returns that part at full resolution |
| `cursor_position` | |
| `mouse_move` | `coordinate` `[x, y]` |
| `left_click`, `right_click`, `middle_click`, `double_click`, `triple_click` | optional `coordinate`; optional `text`: modifiers held during the click, e.g. `"shift"` |
| `left_click_drag` | `start_coordinate`, `coordinate` |
| `left_mouse_down`, `left_mouse_up` | optional `coordinate` |
| `scroll` | `scroll_direction` (`up`, `down`, `left`, `right`), `scroll_amount` (wheel clicks, default 3), optional `coordinate` and modifier `text` |
| `type` | `text`: any Unicode, up to 4096 characters |
| `key` | `text`: a key or chord by X keysym name, e.g. `Return`, `ctrl+l`, `ctrl+shift+Tab`, `F5` |
| `hold_key` | `text`, `duration` (seconds, up to 30) |
| `wait` | `duration` (seconds, up to 60) |

Coordinates are pixels from the top left of the screen and must lie on it.
Set `"screenshot": true` to get a screenshot after the action (after a
300 ms settle), base64 in `image.data` with `image.mime_type`; `format` and
`quality` choose its encoding. `screenshot` and `zoom` always return one.

```sh
id=$(acctl create -image desktop -q)
acctl computer "$id" left_click '{"coordinate": [640, 60]}'
acctl computer "$id" type '{"text": "example.com\n"}'
acctl screenshot -o screen.png "$id"
```

## Snapshots

A snapshot captures a sandbox's memory and disk. `POST /v1/sandboxes` with
`from_snapshot` forks it: the fork resumes exactly where the source was, with
its own hostname, clock and entropy. Snapshots do not survive an acd restart.

| Method and path | Response |
| --- | --- |
| `GET /v1/snapshots` | `{"snapshots": [Snapshot]}` |
| `DELETE /v1/snapshots/{id}` | `204` |

## Example

```sh
export AC_URL=https://agents.example.com AC_API_KEY=ac_…
id=$(acctl create -q)
acctl sh "$id" 'echo hello from $(hostname)'
echo 'print(2+2)' | acctl put "$id" - /work/main.py
acctl exec "$id" -- python3 /work/main.py
acctl rm "$id"
```
