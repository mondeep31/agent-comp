# Deploying acd

acd runs on one Linux host with `/dev/kvm`. It listens on loopback only;
agents on the public internet reach it through a Cloudflare Tunnel, which is
an outbound-only connection from the host, so the host needs no public IP or
open ports. On the dev host, `comp-agents`, SSH goes through Tailscale.

```
agent ──HTTPS──▶ Cloudflare edge ◀──QUIC (outbound)── cloudflared ──▶ 127.0.0.1:8080 acd ──▶ Firecracker VMs
```

## 1. Host prerequisites

- Linux with KVM (`ls -l /dev/kvm`), cgroup v2, `nft`, `ip`, `debootstrap`.
- Firecracker and jailer v1.17 in `/usr/local/bin`.
- `cloudflared` (Cloudflare's apt repository).
- Go and Rust toolchains to build from source (`make build`).

## 2. Build and install

From a checkout on the host:

```sh
make build            # bin/acd, bin/acctl, agentd
make image            # /var/lib/agent-comp/images/{vmlinux,base.ext4} (sudo)
sudo deploy/install.sh
```

`install.sh` is safe to re-run. It installs the binaries and the systemd
units, creates `/etc/agent-comp/acd.yaml` from `deploy/acd.example.yaml` and
an empty `/etc/agent-comp/api-keys` if they do not exist, restarts acd and
waits for `/healthz`. It never overwrites an existing config, key file or
tunnel token.

The first create for each (image, shape) builds a golden snapshot (about
3.5 s). acd builds the default shape at startup and fills the warm pool, so
creates are warm by the time the first agent connects.

## 3. API keys

```sh
sudo acctl keygen -append /etc/agent-comp/api-keys alice
sudo systemctl reload acd      # SIGHUP: reload keys without dropping VMs
```

`keygen` prints the key once and appends only its SHA-256 digest. The key's
name is the owner of everything it creates. To revoke a key, delete its line
and reload.

## 4. Cloudflare Tunnel

You need a domain on Cloudflare.

1. In the Cloudflare dashboard, open **Zero Trust → Networks → Tunnels**,
   create a tunnel of type **Cloudflared** and copy its token.
2. Add a **public hostname**, for example `computers.example.com`, with
   service `http://127.0.0.1:8080`. Do not route the metrics port (9464).
3. Store the token on the host, readable by root only, without it touching
   shell history or the repository:

   ```sh
   sudo install -m 0600 /dev/null /etc/agent-comp/tunnel-token
   sudoedit /etc/agent-comp/tunnel-token     # paste the token
   sudo deploy/install.sh                    # enables agent-comp-tunnel.service
   ```

`agent-comp-tunnel.service` runs cloudflared as a dynamic user and receives
the token as a systemd credential, so it is never in the unit file, the
process arguments or the environment.

Under **Network** in the zone settings, keep WebSockets enabled (the default).
Cloudflare closes connections that are idle for 100 s; acd pings WebSocket
exec sessions every 20 s, and MCP sends keepalives and progress notifications,
so long silent commands survive.

### Quick tunnel for testing

Without a domain, a throwaway `trycloudflare.com` URL works for testing. It
has no uptime guarantee and changes on every start:

```sh
sudo systemd-run --unit ac-quick-tunnel -p DynamicUser=yes \
  cloudflared --no-autoupdate tunnel --metrics 127.0.0.1:9466 --url http://127.0.0.1:8080
journalctl -u ac-quick-tunnel | grep trycloudflare.com
sudo systemctl stop ac-quick-tunnel
```

## 5. Verify

```sh
sudo AC_API_KEY_FILE=/etc/agent-comp/dev-token LONG=1 \
  scripts/host/smoke-api.sh https://computers.example.com
```

The script reads the key from a file (it is never put on a command line) and
checks auth, create, WebSocket exec, egress, the metadata block, `pip
install`, file upload, a 130 s silent command (with `LONG=1`) and an MCP tool
call.

Clients then use:

```sh
export AC_URL=https://computers.example.com AC_API_KEY=...
acctl create
```

or connect an MCP client to `https://computers.example.com/v1/mcp` (see
[MCP.md](MCP.md)).

## CI

| Workflow | Runs on | Triggers | What |
| --- | --- | --- | --- |
| `ci.yml` | GitHub-hosted | push to main, PRs, weekly, manual | `make lint-go test-go build-go`, `lint-rust test-rust build-agentd`, `lint-proto`, `audit` |
| `integration.yml` | self-hosted `kvm` | push to main (code changes), manual | `make build-agentd integration`; any skipped test fails the job |
| `release.yml` | GitHub-hosted | `v*` tags | `make build`, tarball with binaries, `deploy/` and `images/`, SHA-256, GitHub release |

The integration job needs real KVM, so it runs on a self-hosted runner on the
dev host. To register one:

```sh
gh api -X POST repos/OWNER/REPO/actions/runners/registration-token -q .token \
  | ssh HOST 'sudo bash agent-comp/scripts/host/install-runner.sh OWNER/REPO'
```

The script creates an `ac-runner` user with its own Rust toolchain and
passwordless sudo, installs a checksum-verified runner release as a systemd
service and labels it `kvm`. The host also needs `/var/lib/agent-comp/ci`
(`scripts/host/fetch-ci-artifacts.sh`), Firecracker and Go in
`/usr/local/go`. Jobs on this runner are effectively root on the host, so
the workflow never runs for pull requests, and the repository should stay
private or require approval for outside contributors.

Integration tests use their own network instance (`t`), cgroup parents and
uid ranges, so they run safely next to the production acd.

## Operations

| Task | Command |
| --- | --- |
| Logs | `journalctl -u acd -f` (JSON, one request id per line) |
| Metrics | `curl -s 127.0.0.1:9464/metrics` on the host |
| Reload keys | `sudo systemctl reload acd` |
| Upgrade | `git pull && make build && sudo deploy/install.sh` |
| Tunnel status | `systemctl status agent-comp-tunnel`, `curl -s 127.0.0.1:9465/ready` |

Restarting acd destroys all sandboxes and user snapshots: in v1 they live
only as long as the process. Golden snapshots are kept. Changing an image or its boot args
changes the golden fingerprint, and acd rebuilds that golden snapshot on the
next start.
