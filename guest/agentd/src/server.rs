//! Connection dispatch: one operation per connection.

use std::collections::HashMap;
use std::sync::{Arc, RwLock};
use std::time::Instant;

use tokio::io::{AsyncRead, AsyncWrite};

use crate::codec::{read_frame, write_frame};
use crate::error::AgentError;
use crate::proto::frame::Kind;
use crate::proto::{Ack, Pong};
use crate::{configure, exec, files};

/// Default environment for commands, before host-provided overrides.
const BASE_ENV: &[(&str, &str)] = &[
    (
        "PATH",
        "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
    ),
    ("HOME", "/root"),
    ("LANG", "C.UTF-8"),
];

/// Limits and shared state for all connections.
#[derive(Debug)]
pub struct State {
    started: Instant,
    env: RwLock<HashMap<String, String>>,
    /// Largest file `ReadFile`/`WriteFile` transfer.
    pub max_file_bytes: u64,
}

impl Default for State {
    fn default() -> Self {
        Self::new(64 << 20)
    }
}

impl State {
    #[must_use]
    pub fn new(max_file_bytes: u64) -> Self {
        let env = BASE_ENV
            .iter()
            .map(|(k, v)| ((*k).to_owned(), (*v).to_owned()))
            .collect();
        Self {
            started: Instant::now(),
            env: RwLock::new(env),
            max_file_bytes,
        }
    }

    /// The environment every exec starts from.
    pub fn env(&self) -> HashMap<String, String> {
        self.env.read().map(|e| e.clone()).unwrap_or_default()
    }

    /// Merges host-provided defaults into the base environment.
    pub fn set_env(&self, overrides: HashMap<String, String>) {
        if let Ok(mut env) = self.env.write() {
            env.extend(overrides);
        }
    }

    fn uptime_ms(&self) -> u64 {
        u64::try_from(self.started.elapsed().as_millis()).unwrap_or(u64::MAX)
    }
}

/// Serves one connection: reads the opening frame and runs that operation.
pub async fn handle<S>(mut stream: S, state: Arc<State>)
where
    S: AsyncRead + AsyncWrite + Unpin + Send + 'static,
{
    let result = match read_frame(&mut stream).await {
        Ok(Some(kind)) => dispatch(stream, kind, state).await,
        Ok(None) => return,
        Err(e) => Err((stream, AgentError::from(e))),
    };
    if let Err((mut stream, err)) = result {
        let _ = write_frame(&mut stream, Kind::Error(err.to_proto())).await;
    }
}

/// Returns the stream with the error so the caller can report it.
async fn dispatch<S>(mut stream: S, kind: Kind, state: Arc<State>) -> Result<(), (S, AgentError)>
where
    S: AsyncRead + AsyncWrite + Unpin + Send + 'static,
{
    let reply = match kind {
        Kind::Ping(_) => Ok(Kind::Pong(Pong {
            agent_version: crate::VERSION.to_owned(),
            uptime_ms: state.uptime_ms(),
        })),
        Kind::ExecStart(start) => {
            exec::run(stream, start, &state).await;
            return Ok(());
        }
        Kind::ReadFile(req) => return files::read(stream, req, &state).await,
        Kind::WriteFile(req) => return files::write(stream, req, &state).await,
        Kind::Stat(req) => files::stat(&req.path).await.map(Kind::FileInfo),
        Kind::ListDir(req) => files::list(&req.path).await.map(Kind::DirListing),
        Kind::Configure(req) => configure::apply(req, &state)
            .await
            .map(|()| Kind::Ack(Ack {})),
        other => Err(AgentError::invalid(format!(
            "unexpected opening frame: {}",
            kind_name(&other)
        ))),
    };
    match reply {
        Ok(kind) => {
            let _ = write_frame(&mut stream, kind).await;
            Ok(())
        }
        Err(e) => Err((stream, e)),
    }
}

fn kind_name(kind: &Kind) -> &'static str {
    match kind {
        Kind::Error(_) => "Error",
        Kind::Ping(_) => "Ping",
        Kind::Pong(_) => "Pong",
        Kind::ExecStart(_) => "ExecStart",
        Kind::ExecStdin(_) => "ExecStdin",
        Kind::ExecSignal(_) => "ExecSignal",
        Kind::ExecOutput(_) => "ExecOutput",
        Kind::ExecExit(_) => "ExecExit",
        Kind::ReadFile(_) => "ReadFile",
        Kind::WriteFile(_) => "WriteFile",
        Kind::FileData(_) => "FileData",
        Kind::FileInfo(_) => "FileInfo",
        Kind::Stat(_) => "Stat",
        Kind::ListDir(_) => "ListDir",
        Kind::DirListing(_) => "DirListing",
        Kind::Configure(_) => "Configure",
        Kind::Ack(_) => "Ack",
    }
}

/// Accepts vsock connections forever.
#[cfg(target_os = "linux")]
pub async fn serve_vsock(port: u32, state: Arc<State>) -> anyhow::Result<()> {
    use anyhow::Context;
    use tokio_vsock::{VMADDR_CID_ANY, VsockAddr, VsockListener};

    let listener = VsockListener::bind(VsockAddr::new(VMADDR_CID_ANY, port))
        .with_context(|| format!("binding vsock port {port}"))?;
    loop {
        match listener.accept().await {
            Ok((stream, _)) => {
                tokio::spawn(handle(stream, Arc::clone(&state)));
            }
            Err(e) => {
                eprintln!("agentd: accept: {e}");
                tokio::time::sleep(std::time::Duration::from_millis(50)).await;
            }
        }
    }
}

#[cfg(test)]
pub(crate) mod testutil {
    use super::*;

    /// Runs `handle` on one end of an in-memory pipe and returns the other end.
    pub fn connect(state: Arc<State>) -> tokio::io::DuplexStream {
        let (client, server) = tokio::io::duplex(1 << 20);
        tokio::spawn(handle(server, state));
        client
    }

    /// Sends one frame and collects every reply frame until the agent closes.
    pub async fn roundtrip(state: Arc<State>, first: Kind) -> Vec<Kind> {
        let mut c = connect(state);
        write_frame(&mut c, first).await.unwrap();
        let mut out = Vec::new();
        while let Some(k) = read_frame(&mut c).await.unwrap() {
            out.push(k);
        }
        out
    }
}

#[cfg(test)]
mod tests {
    use super::testutil::roundtrip;
    use super::*;
    use crate::proto::{ErrorCode, Ping};

    #[tokio::test]
    async fn ping_returns_version() {
        let frames = roundtrip(Arc::new(State::default()), Kind::Ping(Ping {})).await;
        let [Kind::Pong(pong)] = frames.as_slice() else {
            panic!("unexpected frames: {frames:?}");
        };
        assert_eq!(pong.agent_version, crate::VERSION);
    }

    #[tokio::test]
    async fn reply_frame_as_opening_frame_is_rejected() {
        let frames = roundtrip(Arc::new(State::default()), Kind::Ack(Ack {})).await;
        let [Kind::Error(err)] = frames.as_slice() else {
            panic!("unexpected frames: {frames:?}");
        };
        assert_eq!(err.code(), ErrorCode::InvalidArgument);
    }

    #[test]
    fn set_env_overrides_base() {
        let state = State::default();
        state.set_env(HashMap::from([("PATH".to_owned(), "/x".to_owned())]));
        assert_eq!(state.env().get("PATH").map(String::as_str), Some("/x"));
        assert_eq!(state.env().get("HOME").map(String::as_str), Some("/root"));
    }
}
