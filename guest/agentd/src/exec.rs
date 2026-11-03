//! `Exec`: run a process, stream its output, forward stdin and signals.
//!
//! The process gets its own process group so timeouts, signals and host
//! disconnects reach everything it spawned.

use std::os::unix::process::ExitStatusExt;
use std::path::Path;
use std::process::Stdio;
use std::time::{Duration, Instant};

use bytes::Bytes;
use nix::sys::signal::{Signal, killpg};
use nix::unistd::Pid;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::process::{Child, ChildStdin, Command};
use tokio::sync::{mpsc, oneshot};

use crate::codec::{read_frame, write_frame};
use crate::error::AgentError;
use crate::proto::frame::Kind;
use crate::proto::{ExecExit, ExecOutput, ExecStart, Stream};
use crate::server::State;

const READ_CHUNK: usize = 32 << 10;
/// How long output is still collected after the process exits. Background
/// children that keep the pipes open are cut off after this.
const DRAIN_GRACE: Duration = Duration::from_millis(200);

/// Runs one `ExecStart` to completion on `stream`.
pub async fn run<S>(stream: S, start: ExecStart, state: &State)
where
    S: AsyncRead + AsyncWrite + Unpin + Send + 'static,
{
    let (rd, mut wr) = tokio::io::split(stream);
    let mut child = match spawn(&start, state) {
        Ok(child) => child,
        Err(e) => {
            let _ = write_frame(&mut wr, Kind::Error(e.to_proto())).await;
            return;
        }
    };
    let pgid = child.id().map(|id| id as i32);
    let started = Instant::now();

    let (tx, mut rx) = mpsc::channel::<Kind>(64);
    let writer = tokio::spawn(async move {
        while let Some(kind) = rx.recv().await {
            if write_frame(&mut wr, kind).await.is_err() {
                break;
            }
        }
        let _ = wr.shutdown().await;
    });

    let mut pumps = Vec::new();
    if let Some(out) = child.stdout.take() {
        pumps.push(tokio::spawn(pump(out, Stream::Stdout, tx.clone())));
    }
    if let Some(err) = child.stderr.take() {
        pumps.push(tokio::spawn(pump(err, Stream::Stderr, tx.clone())));
    }

    let (gone_tx, gone_rx) = oneshot::channel::<()>();
    let input = tokio::spawn(input_loop(rd, child.stdin.take(), pgid, gone_tx));

    let timeout =
        (start.timeout_ms > 0).then(|| Duration::from_millis(u64::from(start.timeout_ms)));
    let (status, timed_out) = tokio::select! {
        status = child.wait() => (status, false),
        () = sleep_opt(timeout) => {
            kill_group(pgid, Signal::SIGKILL);
            (child.wait().await, true)
        }
        _ = gone_rx => {
            // The host went away; nobody will read the result.
            kill_group(pgid, Signal::SIGKILL);
            (child.wait().await, false)
        }
    };
    input.abort();

    let aborts: Vec<_> = pumps
        .iter()
        .map(tokio::task::JoinHandle::abort_handle)
        .collect();
    let drained = tokio::time::timeout(DRAIN_GRACE, async {
        for p in pumps {
            let _ = p.await;
        }
    })
    .await;
    if drained.is_err() {
        aborts.iter().for_each(tokio::task::AbortHandle::abort);
    }

    let exit = match status {
        Ok(s) => ExecExit {
            exit_code: s.code().unwrap_or(-1),
            signal: s.signal().unwrap_or(0),
            timed_out,
            duration_ms: u64::try_from(started.elapsed().as_millis()).unwrap_or(u64::MAX),
        },
        Err(e) => {
            let err = AgentError::io("waiting for process", &e);
            let _ = tx.send(Kind::Error(err.to_proto())).await;
            drop(tx);
            let _ = writer.await;
            return;
        }
    };
    let _ = tx.send(Kind::ExecExit(exit)).await;
    drop(tx);
    let _ = writer.await;
}

fn spawn(start: &ExecStart, state: &State) -> Result<Child, AgentError> {
    let Some((program, args)) = start.argv.split_first() else {
        return Err(AgentError::invalid("argv must not be empty"));
    };
    let cwd = if start.cwd.is_empty() {
        default_cwd()
    } else {
        start.cwd.clone()
    };
    let mut cmd = Command::new(program);
    cmd.args(args)
        .env_clear()
        .envs(state.env())
        .envs(&start.env)
        .current_dir(&cwd)
        .stdin(if start.stdin {
            Stdio::piped()
        } else {
            Stdio::null()
        })
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .process_group(0)
        .kill_on_drop(true);
    cmd.spawn()
        .map_err(|e| AgentError::io(&format!("starting {program} in {cwd}"), &e))
}

fn default_cwd() -> String {
    if Path::new("/root").is_dir()
        && nix::unistd::access("/root", nix::unistd::AccessFlags::X_OK).is_ok()
    {
        "/root".to_owned()
    } else {
        "/".to_owned()
    }
}

async fn pump<R: AsyncRead + Unpin>(mut r: R, stream: Stream, tx: mpsc::Sender<Kind>) {
    let mut buf = vec![0u8; READ_CHUNK];
    loop {
        match r.read(&mut buf).await {
            Ok(0) | Err(_) => return,
            Ok(n) => {
                let out = ExecOutput {
                    stream: stream.into(),
                    data: Bytes::copy_from_slice(&buf[..n]),
                };
                if tx.send(Kind::ExecOutput(out)).await.is_err() {
                    return;
                }
            }
        }
    }
}

/// Forwards stdin and signals until the host closes its side.
async fn input_loop<R: AsyncRead + Unpin>(
    mut rd: R,
    mut stdin: Option<ChildStdin>,
    pgid: Option<i32>,
    gone: oneshot::Sender<()>,
) {
    loop {
        match read_frame(&mut rd).await {
            Ok(Some(Kind::ExecStdin(input))) => {
                if let Some(w) = stdin.as_mut()
                    && !input.data.is_empty()
                    && w.write_all(&input.data).await.is_err()
                {
                    stdin = None;
                }
                if input.eof {
                    stdin = None;
                }
            }
            Ok(Some(Kind::ExecSignal(sig))) => {
                if let Ok(signal) = Signal::try_from(sig.signal) {
                    kill_group(pgid, signal);
                }
            }
            Ok(Some(_)) => {}
            Ok(None) | Err(_) => {
                let _ = gone.send(());
                return;
            }
        }
    }
}

fn kill_group(pgid: Option<i32>, signal: Signal) {
    if let Some(pgid) = pgid {
        let _ = killpg(Pid::from_raw(pgid), signal);
    }
}

async fn sleep_opt(d: Option<Duration>) {
    match d {
        Some(d) => tokio::time::sleep(d).await,
        None => std::future::pending().await,
    }
}

#[cfg(test)]
mod tests {
    use std::collections::HashMap;
    use std::sync::Arc;

    use super::*;
    use crate::proto::{ExecSignal, ExecStdin};
    use crate::server::testutil::{connect, roundtrip};

    fn sh(script: &str) -> ExecStart {
        ExecStart {
            argv: vec!["/bin/sh".into(), "-c".into(), script.into()],
            ..ExecStart::default()
        }
    }

    struct Collected {
        stdout: Vec<u8>,
        stderr: Vec<u8>,
        exit: Option<ExecExit>,
    }

    fn collect(frames: Vec<Kind>) -> Collected {
        let mut c = Collected {
            stdout: Vec::new(),
            stderr: Vec::new(),
            exit: None,
        };
        for f in frames {
            match f {
                Kind::ExecOutput(o) if o.stream() == Stream::Stdout => {
                    c.stdout.extend_from_slice(&o.data);
                }
                Kind::ExecOutput(o) => c.stderr.extend_from_slice(&o.data),
                Kind::ExecExit(e) => c.exit = Some(e),
                other => panic!("unexpected frame {other:?}"),
            }
        }
        c
    }

    async fn exec(start: ExecStart) -> Collected {
        collect(roundtrip(Arc::new(State::default()), Kind::ExecStart(start)).await)
    }

    #[tokio::test]
    async fn streams_stdout_and_stderr_separately() {
        let c = exec(sh("echo out; echo err >&2")).await;
        assert_eq!(c.stdout, b"out\n");
        assert_eq!(c.stderr, b"err\n");
        assert_eq!(c.exit.unwrap().exit_code, 0);
    }

    #[tokio::test]
    async fn reports_nonzero_exit_code() {
        let c = exec(sh("exit 7")).await;
        assert_eq!(c.exit.unwrap().exit_code, 7);
    }

    #[tokio::test]
    async fn passes_env_and_cwd() {
        let mut start = sh("echo \"$GREETING\"; pwd");
        start.env = HashMap::from([("GREETING".into(), "hi".into())]);
        start.cwd = "/tmp".into();
        let c = exec(start).await;
        let out = String::from_utf8(c.stdout).unwrap();
        assert!(out.starts_with("hi\n"), "{out}");
        assert!(out.trim_end().ends_with("tmp"), "{out}");
    }

    #[tokio::test]
    async fn timeout_kills_the_process_group() {
        let mut start = sh("sleep 30 & sleep 30");
        start.timeout_ms = 100;
        let c = exec(start).await;
        let exit = c.exit.unwrap();
        assert!(exit.timed_out);
        assert_eq!(exit.signal, 9);
        assert!(exit.duration_ms < 5_000);
    }

    #[tokio::test]
    async fn missing_binary_is_not_found() {
        let start = ExecStart {
            argv: vec!["/definitely/not/here".into()],
            ..ExecStart::default()
        };
        let frames = roundtrip(Arc::new(State::default()), Kind::ExecStart(start)).await;
        let [Kind::Error(err)] = frames.as_slice() else {
            panic!("unexpected frames: {frames:?}");
        };
        assert_eq!(err.code(), crate::proto::ErrorCode::NotFound);
    }

    #[tokio::test]
    async fn forwards_stdin_and_signals() {
        let mut c = connect(Arc::new(State::default()));
        let mut start = sh("read line; echo \"got $line\"; sleep 30");
        start.stdin = true;
        write_frame(&mut c, Kind::ExecStart(start)).await.unwrap();
        write_frame(
            &mut c,
            Kind::ExecStdin(ExecStdin {
                data: Bytes::from_static(b"hello\n"),
                eof: true,
            }),
        )
        .await
        .unwrap();

        let Some(Kind::ExecOutput(out)) = read_frame(&mut c).await.unwrap() else {
            panic!("expected output");
        };
        assert_eq!(&out.data[..], b"got hello\n");

        write_frame(&mut c, Kind::ExecSignal(ExecSignal { signal: 15 }))
            .await
            .unwrap();
        let mut exit = None;
        while let Some(k) = read_frame(&mut c).await.unwrap() {
            if let Kind::ExecExit(e) = k {
                exit = Some(e);
            }
        }
        let exit = exit.unwrap();
        assert_eq!(exit.signal, 15);
        assert!(!exit.timed_out);
    }
}
