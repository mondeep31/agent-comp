use std::sync::Arc;

use anyhow::{Context, Result};

fn main() -> Result<()> {
    // The kernel passes unrecognized boot parameters to init as argv.
    if std::process::id() == 1 {
        return run_init();
    }
    let arg = std::env::args().nth(1);
    match arg.as_deref() {
        Some("--version") => {
            println!("agentd {}", agentd::VERSION);
            Ok(())
        }
        Some("serve") => serve(),
        Some("display") => display(std::env::args().nth(2).as_deref()),
        _ => {
            eprintln!("usage: agentd [serve | display WxH | --version]  (runs as init when PID 1)");
            std::process::exit(2);
        }
    }
}

#[cfg(target_os = "linux")]
fn run_init() -> Result<()> {
    agentd::init::run_pid1()
}

#[cfg(not(target_os = "linux"))]
fn run_init() -> Result<()> {
    anyhow::bail!("init mode is only supported on Linux")
}

#[cfg(target_os = "linux")]
fn display(size: Option<&str>) -> Result<()> {
    let cfg = size
        .and_then(agentd::display::DisplayConfig::parse)
        .context("usage: agentd display WxH")?;
    agentd::display::session::run(cfg)
}

#[cfg(not(target_os = "linux"))]
fn display(_size: Option<&str>) -> Result<()> {
    anyhow::bail!("the display session is only supported on Linux")
}

fn serve() -> Result<()> {
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .context("building runtime")?;
    let display = std::fs::read_to_string("/proc/cmdline")
        .ok()
        .and_then(|c| agentd::display::DisplayConfig::from_cmdline(&c));
    let state = Arc::new(agentd::server::State::default().with_display(display));
    rt.block_on(listen(state))
}

#[cfg(target_os = "linux")]
async fn listen(state: Arc<agentd::server::State>) -> Result<()> {
    agentd::server::serve_vsock(agentd::VSOCK_PORT, state).await
}

#[cfg(not(target_os = "linux"))]
async fn listen(_state: Arc<agentd::server::State>) -> Result<()> {
    anyhow::bail!("vsock is only supported on Linux")
}
