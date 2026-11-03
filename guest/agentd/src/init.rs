//! PID 1 duties: build the root filesystem, then supervise the server.
//!
//! The root device (`/dev/vda`) is the image's shared read-only rootfs. The
//! second disk (`/dev/vdb`) is the VM's private ext4 overlay. PID 1 mounts an
//! overlayfs of the two, pivots into it, mounts the API filesystems, brings up
//! networking from the kernel command line, and then only reaps processes:
//! the server runs as a child so its own `waitpid` calls never race the reaper.

use std::fs;
use std::path::Path;
use std::process::Command;
use std::time::Duration;

use anyhow::{Context, Result};
use nix::errno::Errno;
use nix::mount::{MntFlags, MsFlags, mount, umount2};
use nix::sys::wait::waitpid;
use nix::unistd::{chdir, pivot_root, sethostname};

const OVERLAY_DEVICE: &str = "/dev/vdb";
const OVERLAY_MOUNT: &str = "/mnt";

/// Never returns under normal operation.
pub fn run_pid1() -> Result<()> {
    mount_dev("/dev")?;
    if Path::new(OVERLAY_DEVICE).exists() {
        pivot_to_overlay().context("building overlay root")?;
    } else {
        eprintln!("agentd: no overlay disk, running on the read-only root");
    }
    mount_api_filesystems()?;
    let _ = sethostname("sandbox");
    setup_network_from_cmdline();
    supervise()
}

fn mount_dev(target: &str) -> Result<()> {
    match mount(
        Some("devtmpfs"),
        target,
        Some("devtmpfs"),
        MsFlags::MS_NOSUID,
        Some("mode=0755"),
    ) {
        Ok(()) | Err(Errno::EBUSY) => Ok(()),
        Err(e) => Err(e).context("mounting devtmpfs"),
    }
}

fn pivot_to_overlay() -> Result<()> {
    mount(
        None::<&str>,
        "/",
        None::<&str>,
        MsFlags::MS_REC | MsFlags::MS_PRIVATE,
        None::<&str>,
    )
    .context("making / private")?;
    mount(
        Some(OVERLAY_DEVICE),
        OVERLAY_MOUNT,
        Some("ext4"),
        MsFlags::MS_NOATIME,
        None::<&str>,
    )
    .context("mounting overlay disk")?;

    let upper = format!("{OVERLAY_MOUNT}/upper");
    let work = format!("{OVERLAY_MOUNT}/work");
    let merged = format!("{OVERLAY_MOUNT}/merged");
    for dir in [&upper, &work, &merged] {
        fs::create_dir_all(dir).with_context(|| format!("creating {dir}"))?;
    }
    let opts = format!("lowerdir=/,upperdir={upper},workdir={work}");
    mount(
        Some("overlay"),
        merged.as_str(),
        Some("overlay"),
        MsFlags::empty(),
        Some(opts.as_str()),
    )
    .context("mounting overlayfs")?;

    // pivot_root(".", ".") stacks the old root on top of the new one; detaching
    // it leaves only the overlay. See pivot_root(2), NOTES.
    chdir(merged.as_str())?;
    pivot_root(".", ".").context("pivot_root")?;
    umount2(".", MntFlags::MNT_DETACH).context("detaching old root")?;
    chdir("/")?;
    Ok(())
}

fn mount_api_filesystems() -> Result<()> {
    let nosuid_nodev_noexec = MsFlags::MS_NOSUID | MsFlags::MS_NODEV | MsFlags::MS_NOEXEC;
    let mounts: [(&str, &str, &str, MsFlags, Option<&str>); 7] = [
        ("proc", "/proc", "proc", nosuid_nodev_noexec, None),
        ("sysfs", "/sys", "sysfs", nosuid_nodev_noexec, None),
        (
            "devpts",
            "/dev/pts",
            "devpts",
            MsFlags::MS_NOSUID | MsFlags::MS_NOEXEC,
            Some("ptmxmode=0666"),
        ),
        (
            "tmpfs",
            "/dev/shm",
            "tmpfs",
            MsFlags::MS_NOSUID | MsFlags::MS_NODEV,
            Some("mode=1777"),
        ),
        (
            "tmpfs",
            "/run",
            "tmpfs",
            MsFlags::MS_NOSUID | MsFlags::MS_NODEV,
            Some("mode=0755"),
        ),
        (
            "tmpfs",
            "/tmp",
            "tmpfs",
            MsFlags::MS_NOSUID | MsFlags::MS_NODEV,
            Some("mode=1777"),
        ),
        (
            "cgroup2",
            "/sys/fs/cgroup",
            "cgroup2",
            nosuid_nodev_noexec,
            None,
        ),
    ];
    mount_dev("/dev")?;
    for (source, target, fstype, flags, data) in mounts {
        let _ = fs::create_dir_all(target);
        match mount(Some(source), target, Some(fstype), flags, data) {
            Ok(()) | Err(Errno::EBUSY) => {}
            Err(e) => eprintln!("agentd: mounting {target}: {e}"),
        }
    }
    Ok(())
}

/// Reads `ac.ip=`, `ac.gw=` and `ac.dns=` (comma separated) from /proc/cmdline.
fn setup_network_from_cmdline() {
    let _ = Command::new("ip")
        .args(["link", "set", "lo", "up"])
        .status();
    let Ok(cmdline) = fs::read_to_string("/proc/cmdline") else {
        return;
    };
    let net = parse_cmdline_network(&cmdline);
    if net.address_cidr.is_empty() {
        return;
    }
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build();
    let result = rt.map_err(anyhow::Error::from).and_then(|rt| {
        rt.block_on(crate::configure::apply_network(&net))
            .map_err(anyhow::Error::from)
    });
    if let Err(e) = result {
        eprintln!("agentd: network setup: {e}");
    }
}

pub fn parse_cmdline_network(cmdline: &str) -> crate::proto::NetworkConfig {
    let mut net = crate::proto::NetworkConfig {
        interface: "eth0".into(),
        ..Default::default()
    };
    for arg in cmdline.split_whitespace() {
        if let Some(v) = arg.strip_prefix("ac.ip=") {
            v.clone_into(&mut net.address_cidr);
        } else if let Some(v) = arg.strip_prefix("ac.gw=") {
            v.clone_into(&mut net.gateway);
        } else if let Some(v) = arg.strip_prefix("ac.dns=") {
            net.nameservers = v
                .split(',')
                .filter(|s| !s.is_empty())
                .map(str::to_owned)
                .collect();
        }
    }
    net
}

/// Runs the server as a child and reaps every process that ends up with PID 1.
fn supervise() -> Result<()> {
    loop {
        let server = Command::new("/proc/self/exe")
            .arg("serve")
            .spawn()
            .context("starting server")?;
        let server_pid = nix::unistd::Pid::from_raw(server.id() as i32);
        loop {
            match waitpid(None, None) {
                Ok(status) if status.pid() == Some(server_pid) => {
                    eprintln!("agentd: server exited ({status:?}); restarting");
                    break;
                }
                Ok(_) | Err(Errno::EINTR) => {}
                Err(Errno::ECHILD) => break,
                Err(e) => eprintln!("agentd: waitpid: {e}"),
            }
        }
        std::thread::sleep(Duration::from_millis(100));
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_network_from_cmdline() {
        let net = parse_cmdline_network(
            "console=ttyS0 ac.ip=172.16.0.2/30 ac.gw=172.16.0.1 ac.dns=1.1.1.1,8.8.8.8 quiet",
        );
        assert_eq!(net.address_cidr, "172.16.0.2/30");
        assert_eq!(net.gateway, "172.16.0.1");
        assert_eq!(net.nameservers, vec!["1.1.1.1", "8.8.8.8"]);
    }

    #[test]
    fn no_network_args_leaves_address_empty() {
        assert_eq!(parse_cmdline_network("console=ttyS0").address_cidr, "");
    }
}
