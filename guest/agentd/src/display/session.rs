//! `agentd display WxH`: keeps the X server and the desktop session running.
//!
//! The image supplies the desktop: `/etc/agent-comp/desktop/session` starts
//! the window manager and apps and should keep running, and the optional
//! `/etc/agent-comp/desktop/ready` exits 0 once the desktop is usable. Both
//! run as the image's `agent` user when it exists, otherwise as root. Without
//! a session script the screen is a bare X server.
//!
//! The X server is never restarted while it runs: a crash restarts the whole
//! session after a backoff, and the state file says why.

use std::fs::{self, File, OpenOptions};
use std::os::unix::fs::PermissionsExt;
use std::os::unix::process::CommandExt;
use std::path::Path;
use std::process::{Child, Command, Stdio};
use std::thread::sleep;
use std::time::{Duration, Instant};

use anyhow::{Context, Result};

use super::{DISPLAY, DisplayConfig, STATE_FILE, Status};

const SESSION: &str = "/etc/agent-comp/desktop/session";
const READY: &str = "/etc/agent-comp/desktop/ready";
const SOCKET_DIR: &str = "/tmp/.X11-unix";
const SOCKET: &str = "/tmp/.X11-unix/X0";
const LOG_DIR: &str = "/var/log/agent-comp";
const USER: &str = "agent";

const X_TIMEOUT: Duration = Duration::from_secs(15);
const READY_TIMEOUT: Duration = Duration::from_secs(90);
const POLL: Duration = Duration::from_millis(100);
const MAX_BACKOFF: Duration = Duration::from_secs(30);

/// The account the desktop runs as.
#[derive(Debug, Clone)]
struct Account {
    uid: u32,
    gid: u32,
    home: String,
    name: String,
}

impl Account {
    fn root() -> Self {
        Self {
            uid: 0,
            gid: 0,
            home: "/root".into(),
            name: "root".into(),
        }
    }

    fn lookup(name: &str) -> Option<Self> {
        let passwd = fs::read_to_string("/etc/passwd").ok()?;
        parse_passwd(&passwd, name)
    }

    fn command(&self, program: &str) -> Command {
        let mut cmd = Command::new(program);
        cmd.env_clear()
            .env(
                "PATH",
                "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
            )
            .env("HOME", &self.home)
            .env("USER", &self.name)
            .env("LOGNAME", &self.name)
            .env("LANG", "C.UTF-8")
            .env("DISPLAY", DISPLAY)
            .current_dir(&self.home)
            .stdin(Stdio::null())
            // Own process group, so a session can be killed as a whole.
            .process_group(0);
        if self.uid != 0 {
            cmd.uid(self.uid).gid(self.gid);
        }
        cmd
    }
}

fn parse_passwd(passwd: &str, name: &str) -> Option<Account> {
    passwd.lines().find_map(|line| {
        let f: Vec<&str> = line.split(':').collect();
        (f.len() >= 7 && f[0] == name).then(|| {
            Some(Account {
                uid: f[2].parse().ok()?,
                gid: f[3].parse().ok()?,
                home: f[5].to_owned(),
                name: f[0].to_owned(),
            })
        })?
    })
}

/// Runs forever, restarting the session when it fails.
pub fn run(cfg: DisplayConfig) -> Result<()> {
    let account = Account::lookup(USER).unwrap_or_else(Account::root);
    prepare(&account)?;
    let mut backoff = Duration::from_millis(500);
    loop {
        let started = Instant::now();
        let err = run_once(cfg, &account);
        let msg = format!("{err:#}");
        eprintln!("agentd: display: {msg}; restarting in {backoff:?}");
        publish(&Status {
            ready: false,
            error: msg,
        });
        sleep(backoff);
        // A session that ran for a while earns a fresh, short backoff.
        backoff = if started.elapsed() > MAX_BACKOFF {
            Duration::from_millis(500)
        } else {
            (backoff * 2).min(MAX_BACKOFF)
        };
    }
}

fn prepare(account: &Account) -> Result<()> {
    fs::create_dir_all(SOCKET_DIR).context("creating X socket dir")?;
    fs::set_permissions(SOCKET_DIR, fs::Permissions::from_mode(0o1777))
        .context("chmod X socket dir")?;
    fs::create_dir_all(LOG_DIR).context("creating log dir")?;
    if let Some(dir) = Path::new(STATE_FILE).parent() {
        fs::create_dir_all(dir).context("creating state dir")?;
    }
    if account.uid != 0 {
        let _ = std::os::unix::fs::chown(&account.home, Some(account.uid), Some(account.gid));
    }
    Ok(())
}

/// Starts X and the session, then watches them. Only returns on failure.
fn run_once(cfg: DisplayConfig, account: &Account) -> anyhow::Error {
    publish(&Status::default());
    let _ = fs::remove_file(SOCKET);
    let _ = fs::remove_file("/tmp/.X0-lock");

    let mut x = match account
        .command("Xvfb")
        .args([
            DISPLAY,
            "-screen",
            "0",
            &format!("{cfg}x24"),
            "-nolisten",
            "tcp",
            "-dpi",
            "96",
            "-nocursor",
        ])
        .stdout(log("xvfb"))
        .stderr(log("xvfb"))
        .spawn()
    {
        Ok(child) => Guard(child),
        Err(e) => return anyhow::anyhow!("starting Xvfb: {e}"),
    };
    if let Err(e) = wait_for(X_TIMEOUT, || {
        exited(&mut x.0, "Xvfb")?;
        Ok(Path::new(SOCKET).exists())
    }) {
        return e.context("waiting for the X server");
    }

    let mut session = if Path::new(SESSION).exists() {
        match account
            .command(SESSION)
            .stdout(log("session"))
            .stderr(log("session"))
            .spawn()
        {
            Ok(child) => Some(Guard(child)),
            Err(e) => return anyhow::anyhow!("starting {SESSION}: {e}"),
        }
    } else {
        None
    };

    let has_probe = Path::new(READY).exists();
    let ready = wait_for(READY_TIMEOUT, || {
        exited(&mut x.0, "Xvfb")?;
        if let Some(s) = session.as_mut() {
            exited(&mut s.0, "the session")?;
        }
        Ok(!has_probe || probe(account))
    });
    if let Err(e) = ready {
        return e.context("waiting for the desktop");
    }
    publish(&Status {
        ready: true,
        error: String::new(),
    });

    loop {
        if let Err(e) = exited(&mut x.0, "Xvfb") {
            return e;
        }
        if let Some(s) = session.as_mut()
            && let Err(e) = exited(&mut s.0, "the session")
        {
            return e;
        }
        sleep(Duration::from_millis(500));
    }
}

/// Kills a child's whole process group when dropped.
struct Guard(Child);

impl Drop for Guard {
    fn drop(&mut self) {
        if let Ok(pid) = i32::try_from(self.0.id()) {
            let _ = nix::sys::signal::killpg(
                nix::unistd::Pid::from_raw(pid),
                nix::sys::signal::Signal::SIGKILL,
            );
        }
        let _ = self.0.wait();
    }
}

fn exited(child: &mut Child, what: &str) -> Result<()> {
    match child.try_wait() {
        Ok(None) => Ok(()),
        Ok(Some(status)) => Err(anyhow::anyhow!("{what} exited ({status})")),
        Err(e) => Err(anyhow::anyhow!("checking {what}: {e}")),
    }
}

fn wait_for(timeout: Duration, mut check: impl FnMut() -> Result<bool>) -> Result<()> {
    let deadline = Instant::now() + timeout;
    loop {
        if check()? {
            return Ok(());
        }
        if Instant::now() >= deadline {
            anyhow::bail!("timed out after {timeout:?}");
        }
        sleep(POLL);
    }
}

fn probe(account: &Account) -> bool {
    account
        .command(READY)
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()
        .is_ok_and(|s| s.success())
}

fn log(name: &str) -> Stdio {
    OpenOptions::new()
        .create(true)
        .append(true)
        .open(format!("{LOG_DIR}/{name}.log"))
        .map_or_else(|_| Stdio::null(), Stdio::from)
}

fn publish(status: &Status) {
    let tmp = format!("{STATE_FILE}.tmp");
    let written = File::create(&tmp)
        .and_then(|mut f| std::io::Write::write_all(&mut f, status.encode().as_bytes()))
        .and_then(|()| fs::rename(&tmp, STATE_FILE));
    if let Err(e) = written {
        eprintln!("agentd: display: writing {STATE_FILE}: {e}");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn finds_account_in_passwd() {
        let passwd = "root:x:0:0:root:/root:/bin/bash\nagent:x:1000:1000::/home/agent:/bin/bash\n";
        let a = parse_passwd(passwd, "agent").expect("agent");
        assert_eq!((a.uid, a.gid, a.home.as_str()), (1000, 1000, "/home/agent"));
        assert!(parse_passwd(passwd, "nobody").is_none());
        assert!(parse_passwd("agent:x:notanumber:1::/h:/bin/sh\n", "agent").is_none());
    }

    #[test]
    fn wait_for_times_out_and_propagates_errors() {
        assert!(wait_for(Duration::from_millis(1), || Ok(false)).is_err());
        assert!(wait_for(Duration::from_secs(1), || Ok(true)).is_ok());
        assert!(wait_for(Duration::from_secs(1), || anyhow::bail!("boom")).is_err());
    }
}
