//! Guest agent for agent-comp microVMs.
//!
//! Runs as PID 1 inside every microVM and serves exec, file and configuration
//! requests from the host over vsock. See `proto/agentcomp/agentd/v1/agentd.proto`
//! for the wire protocol.

pub mod codec;
pub mod configure;
pub mod error;
pub mod exec;
pub mod files;
#[cfg(target_os = "linux")]
pub mod init;
pub mod server;

/// Generated protobuf types for `agentcomp.agentd.v1`.
#[allow(clippy::pedantic, missing_debug_implementations)]
pub mod proto {
    include!(concat!(env!("OUT_DIR"), "/agentcomp.agentd.v1.rs"));
}

/// The vsock port agentd listens on.
pub const VSOCK_PORT: u32 = 1024;

/// Agent version reported in `Pong`.
pub const VERSION: &str = env!("CARGO_PKG_VERSION");
