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
        _ => {
            eprintln!("usage: agentd [serve | --version]  (runs as init when PID 1)");
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

fn serve() -> Result<()> {
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .context("building runtime")?;
    let state = Arc::new(agentd::server::State::default());
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
