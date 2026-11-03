//! `Configure`: per-VM identity applied after boot and after every snapshot
//! restore, so clones of one golden snapshot diverge.

use std::fmt::Write as _;

use crate::error::AgentError;
use crate::proto::{Configure, NetworkConfig};
use crate::server::State;

pub async fn apply(req: Configure, state: &State) -> Result<(), AgentError> {
    if !req.entropy.is_empty() {
        add_entropy(&req.entropy).await?;
    }
    if req.wall_clock_unix_ms > 0 {
        set_clock(req.wall_clock_unix_ms)?;
    }
    if !req.hostname.is_empty() {
        set_hostname(&req.hostname)?;
    }
    if let Some(net) = &req.network {
        apply_network(net).await?;
    }
    if !req.env.is_empty() {
        state.set_env(req.env);
    }
    Ok(())
}

/// Mixes host randomness into the guest pool. Writing does not credit entropy,
/// but it guarantees restored clones stop sharing RNG state.
async fn add_entropy(data: &[u8]) -> Result<(), AgentError> {
    tokio::fs::write("/dev/urandom", data)
        .await
        .map_err(|e| AgentError::io("writing /dev/urandom", &e))
}

#[cfg(target_os = "linux")]
fn set_clock(unix_ms: i64) -> Result<(), AgentError> {
    use nix::sys::time::TimeSpec;
    use nix::time::{ClockId, clock_settime};

    let ts = TimeSpec::new(unix_ms / 1000, (unix_ms % 1000) * 1_000_000);
    clock_settime(ClockId::CLOCK_REALTIME, ts)
        .map_err(|e| AgentError::io("setting clock", &std::io::Error::from(e)))
}

#[cfg(not(target_os = "linux"))]
fn set_clock(_unix_ms: i64) -> Result<(), AgentError> {
    Ok(())
}

#[cfg(target_os = "linux")]
fn set_hostname(name: &str) -> Result<(), AgentError> {
    if name.len() > 63 || !name.chars().all(|c| c.is_ascii_alphanumeric() || c == '-') {
        return Err(AgentError::invalid(format!("invalid hostname {name:?}")));
    }
    nix::unistd::sethostname(name)
        .map_err(|e| AgentError::io("setting hostname", &std::io::Error::from(e)))
}

#[cfg(not(target_os = "linux"))]
fn set_hostname(_name: &str) -> Result<(), AgentError> {
    Ok(())
}

/// Configures an interface with iproute2 and rewrites resolv.conf.
pub async fn apply_network(net: &NetworkConfig) -> Result<(), AgentError> {
    let iface = if net.interface.is_empty() {
        "eth0"
    } else {
        &net.interface
    };
    if !net.address_cidr.is_empty() {
        ip(&["addr", "flush", "dev", iface]).await?;
        ip(&["addr", "add", &net.address_cidr, "dev", iface]).await?;
        ip(&["link", "set", iface, "up"]).await?;
    }
    if !net.gateway.is_empty() {
        ip(&[
            "route",
            "replace",
            "default",
            "via",
            &net.gateway,
            "dev",
            iface,
        ])
        .await?;
    }
    if !net.nameservers.is_empty() {
        let conf = net.nameservers.iter().fold(String::new(), |mut s, ns| {
            let _ = writeln!(s, "nameserver {ns}");
            s
        });
        tokio::fs::write("/etc/resolv.conf", conf)
            .await
            .map_err(|e| AgentError::io("writing /etc/resolv.conf", &e))?;
    }
    Ok(())
}

async fn ip(args: &[&str]) -> Result<(), AgentError> {
    let out = tokio::process::Command::new("ip")
        .args(args)
        .output()
        .await
        .map_err(|e| AgentError::io("running ip", &e))?;
    if out.status.success() {
        return Ok(());
    }
    Err(AgentError::invalid(format!(
        "ip {}: {}",
        args.join(" "),
        String::from_utf8_lossy(&out.stderr).trim()
    )))
}

#[cfg(test)]
mod tests {
    use std::collections::HashMap;

    use super::*;

    #[tokio::test]
    async fn env_is_merged_into_state() {
        let state = State::default();
        let req = Configure {
            env: HashMap::from([("AGENT".into(), "x".into())]),
            ..Configure::default()
        };
        apply(req, &state).await.unwrap();
        assert_eq!(state.env().get("AGENT").map(String::as_str), Some("x"));
    }
}
