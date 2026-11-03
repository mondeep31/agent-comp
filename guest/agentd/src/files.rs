//! File operations: read, write, stat and list.

use std::fs::Metadata;
use std::os::unix::fs::{MetadataExt, PermissionsExt};
use std::path::Path;
use std::time::UNIX_EPOCH;

use bytes::Bytes;
use tokio::fs;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};

use crate::codec::{read_frame, write_frame};
use crate::error::AgentError;
use crate::proto::frame::Kind;
use crate::proto::{DirListing, ErrorCode, FileData, FileInfo, FileType, ReadFile, WriteFile};
use crate::server::State;

const CHUNK: usize = 256 << 10;
const MAX_DIR_ENTRIES: usize = 10_000;

fn require_absolute(path: &str) -> Result<&Path, AgentError> {
    let p = Path::new(path);
    if p.is_absolute() {
        Ok(p)
    } else {
        Err(AgentError::invalid(format!(
            "path must be absolute: {path:?}"
        )))
    }
}

fn file_info(path: &Path, md: &Metadata) -> FileInfo {
    let ft = md.file_type();
    let kind = if ft.is_symlink() {
        FileType::Symlink
    } else if ft.is_dir() {
        FileType::Dir
    } else if ft.is_file() {
        FileType::File
    } else {
        FileType::Other
    };
    let mtime_unix_ms = md
        .modified()
        .ok()
        .and_then(|t| t.duration_since(UNIX_EPOCH).ok())
        .map_or(0, |d| i64::try_from(d.as_millis()).unwrap_or(i64::MAX));
    FileInfo {
        path: path.to_string_lossy().into_owned(),
        name: path
            .file_name()
            .map(|n| n.to_string_lossy().into_owned())
            .unwrap_or_default(),
        r#type: kind.into(),
        size: md.len(),
        mode: md.mode() & 0o7777,
        mtime_unix_ms,
    }
}

pub async fn stat(path: &str) -> Result<FileInfo, AgentError> {
    let p = require_absolute(path)?;
    let md = fs::symlink_metadata(p)
        .await
        .map_err(|e| AgentError::io(&format!("stat {path}"), &e))?;
    Ok(file_info(p, &md))
}

pub async fn list(path: &str) -> Result<DirListing, AgentError> {
    let p = require_absolute(path)?;
    let mut dir = fs::read_dir(p)
        .await
        .map_err(|e| AgentError::io(&format!("listing {path}"), &e))?;
    let mut entries = Vec::new();
    while let Some(entry) = dir
        .next_entry()
        .await
        .map_err(|e| AgentError::io(&format!("listing {path}"), &e))?
    {
        if entries.len() >= MAX_DIR_ENTRIES {
            return Err(AgentError::new(
                ErrorCode::TooLarge,
                format!("{path} has more than {MAX_DIR_ENTRIES} entries"),
            ));
        }
        // Entries can vanish between readdir and stat; skip them.
        if let Ok(md) = entry.metadata().await {
            entries.push(file_info(&entry.path(), &md));
        }
    }
    entries.sort_by(|a, b| a.name.cmp(&b.name));
    Ok(DirListing { entries })
}

/// Streams a file: `FileInfo`, then `FileData` chunks, the last with `eof`.
pub async fn read<S>(mut stream: S, req: ReadFile, state: &State) -> Result<(), (S, AgentError)>
where
    S: AsyncRead + AsyncWrite + Unpin,
{
    let limit = match req.max_bytes {
        0 => state.max_file_bytes,
        n => n.min(state.max_file_bytes),
    };
    let opened = async {
        let p = require_absolute(&req.path)?;
        let file = fs::File::open(p)
            .await
            .map_err(|e| AgentError::io(&format!("opening {}", req.path), &e))?;
        let md = file
            .metadata()
            .await
            .map_err(|e| AgentError::io(&format!("stat {}", req.path), &e))?;
        if md.is_dir() {
            return Err(AgentError::invalid(format!("{} is a directory", req.path)));
        }
        if md.len() > limit {
            return Err(AgentError::new(
                ErrorCode::TooLarge,
                format!("{} is {} bytes, limit is {limit}", req.path, md.len()),
            ));
        }
        Ok((file, file_info(p, &md)))
    }
    .await;
    let (mut file, info) = match opened {
        Ok(v) => v,
        Err(e) => return Err((stream, e)),
    };

    if write_frame(&mut stream, Kind::FileInfo(info))
        .await
        .is_err()
    {
        return Ok(());
    }
    let mut buf = vec![0u8; CHUNK];
    let mut sent: u64 = 0;
    loop {
        let n = match file.read(&mut buf).await {
            Ok(n) => n,
            Err(e) => return Err((stream, AgentError::io(&format!("reading {}", req.path), &e))),
        };
        sent += n as u64;
        // The file may have grown since we checked its size.
        if sent > limit {
            return Err((
                stream,
                AgentError::new(
                    ErrorCode::TooLarge,
                    format!("{} grew past {limit} bytes", req.path),
                ),
            ));
        }
        let data = FileData {
            data: Bytes::copy_from_slice(&buf[..n]),
            eof: n == 0,
        };
        if write_frame(&mut stream, Kind::FileData(data))
            .await
            .is_err()
            || n == 0
        {
            return Ok(());
        }
    }
}

/// Receives `FileData` frames until `eof`, then replies with the new `FileInfo`.
pub async fn write<S>(mut stream: S, req: WriteFile, state: &State) -> Result<(), (S, AgentError)>
where
    S: AsyncRead + AsyncWrite + Unpin,
{
    let result = receive(&mut stream, &req, state).await;
    match result {
        Ok(info) => {
            let _ = write_frame(&mut stream, Kind::FileInfo(info)).await;
            Ok(())
        }
        Err(e) => Err((stream, e)),
    }
}

async fn receive<S>(stream: &mut S, req: &WriteFile, state: &State) -> Result<FileInfo, AgentError>
where
    S: AsyncRead + AsyncWrite + Unpin,
{
    let p = require_absolute(&req.path)?;
    if req.create_parents
        && let Some(parent) = p.parent()
    {
        fs::create_dir_all(parent)
            .await
            .map_err(|e| AgentError::io(&format!("creating parents of {}", req.path), &e))?;
    }
    let mode = if req.mode == 0 {
        0o644
    } else {
        req.mode & 0o7777
    };
    let mut opts = fs::OpenOptions::new();
    opts.write(true).create(true).mode(mode);
    if req.append {
        opts.append(true);
    } else {
        opts.truncate(true);
    }
    let mut file = opts
        .open(p)
        .await
        .map_err(|e| AgentError::io(&format!("opening {} for writing", req.path), &e))?;

    let mut written: u64 = 0;
    loop {
        let Some(kind) = read_frame(stream).await? else {
            return Err(AgentError::invalid("connection closed before eof"));
        };
        let Kind::FileData(chunk) = kind else {
            return Err(AgentError::invalid("expected FileData"));
        };
        written += chunk.data.len() as u64;
        if written > state.max_file_bytes {
            return Err(AgentError::new(
                ErrorCode::TooLarge,
                format!("write exceeds {} bytes", state.max_file_bytes),
            ));
        }
        file.write_all(&chunk.data)
            .await
            .map_err(|e| AgentError::io(&format!("writing {}", req.path), &e))?;
        if chunk.eof {
            break;
        }
    }
    file.flush()
        .await
        .map_err(|e| AgentError::io(&format!("flushing {}", req.path), &e))?;
    if req.mode != 0 {
        // Apply the mode to existing files too; the umask only affects creation.
        fs::set_permissions(p, std::fs::Permissions::from_mode(mode))
            .await
            .map_err(|e| AgentError::io(&format!("chmod {}", req.path), &e))?;
    }
    let md = fs::symlink_metadata(p)
        .await
        .map_err(|e| AgentError::io(&format!("stat {}", req.path), &e))?;
    Ok(file_info(p, &md))
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;

    use super::*;
    use crate::proto::{ListDir, Stat};
    use crate::server::testutil::{connect, roundtrip};

    fn path_in(dir: &tempfile::TempDir, name: &str) -> String {
        dir.path().join(name).to_string_lossy().into_owned()
    }

    async fn write_via_agent(state: Arc<State>, req: WriteFile, chunks: &[&[u8]]) -> Vec<Kind> {
        let mut c = connect(state);
        write_frame(&mut c, Kind::WriteFile(req)).await.unwrap();
        for (i, chunk) in chunks.iter().enumerate() {
            let data = FileData {
                data: Bytes::copy_from_slice(chunk),
                eof: i == chunks.len() - 1,
            };
            write_frame(&mut c, Kind::FileData(data)).await.unwrap();
        }
        let mut out = Vec::new();
        while let Some(k) = read_frame(&mut c).await.unwrap() {
            out.push(k);
        }
        out
    }

    #[tokio::test]
    async fn write_then_read_round_trips() {
        let dir = tempfile::tempdir().unwrap();
        let path = path_in(&dir, "a/b/file.txt");
        let state = Arc::new(State::default());

        let req = WriteFile {
            path: path.clone(),
            mode: 0o600,
            create_parents: true,
            append: false,
        };
        let frames = write_via_agent(Arc::clone(&state), req, &[b"hello ", b"world"]).await;
        let [Kind::FileInfo(info)] = frames.as_slice() else {
            panic!("unexpected frames: {frames:?}");
        };
        assert_eq!(info.size, 11);
        assert_eq!(info.mode, 0o600);

        let frames = roundtrip(state, Kind::ReadFile(ReadFile { path, max_bytes: 0 })).await;
        let mut data = Vec::new();
        for f in &frames[1..] {
            let Kind::FileData(d) = f else {
                panic!("unexpected frame {f:?}");
            };
            data.extend_from_slice(&d.data);
        }
        assert!(matches!(frames[0], Kind::FileInfo(_)));
        assert_eq!(data, b"hello world");
    }

    #[tokio::test]
    async fn read_enforces_size_limit() {
        let dir = tempfile::tempdir().unwrap();
        let path = path_in(&dir, "big");
        std::fs::write(&path, vec![0u8; 100]).unwrap();
        let frames = roundtrip(
            Arc::new(State::new(10)),
            Kind::ReadFile(ReadFile { path, max_bytes: 0 }),
        )
        .await;
        let [Kind::Error(err)] = frames.as_slice() else {
            panic!("unexpected frames: {frames:?}");
        };
        assert_eq!(err.code(), ErrorCode::TooLarge);
    }

    #[tokio::test]
    async fn write_enforces_size_limit() {
        let dir = tempfile::tempdir().unwrap();
        let req = WriteFile {
            path: path_in(&dir, "f"),
            ..WriteFile::default()
        };
        let frames = write_via_agent(Arc::new(State::new(4)), req, &[b"too long"]).await;
        let [Kind::Error(err)] = frames.as_slice() else {
            panic!("unexpected frames: {frames:?}");
        };
        assert_eq!(err.code(), ErrorCode::TooLarge);
    }

    #[tokio::test]
    async fn relative_paths_are_rejected() {
        let frames = roundtrip(
            Arc::new(State::default()),
            Kind::Stat(Stat {
                path: "etc/passwd".into(),
            }),
        )
        .await;
        let [Kind::Error(err)] = frames.as_slice() else {
            panic!("unexpected frames: {frames:?}");
        };
        assert_eq!(err.code(), ErrorCode::InvalidArgument);
    }

    #[tokio::test]
    async fn list_dir_is_sorted_with_types() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("b.txt"), b"x").unwrap();
        std::fs::create_dir(dir.path().join("a")).unwrap();
        let frames = roundtrip(
            Arc::new(State::default()),
            Kind::ListDir(ListDir {
                path: dir.path().to_string_lossy().into_owned(),
            }),
        )
        .await;
        let [Kind::DirListing(listing)] = frames.as_slice() else {
            panic!("unexpected frames: {frames:?}");
        };
        let names: Vec<_> = listing
            .entries
            .iter()
            .map(|e| (e.name.as_str(), e.r#type()))
            .collect();
        assert_eq!(names, vec![("a", FileType::Dir), ("b.txt", FileType::File)]);
    }

    #[tokio::test]
    async fn stat_missing_file_is_not_found() {
        let dir = tempfile::tempdir().unwrap();
        let frames = roundtrip(
            Arc::new(State::default()),
            Kind::Stat(Stat {
                path: path_in(&dir, "nope"),
            }),
        )
        .await;
        let [Kind::Error(err)] = frames.as_slice() else {
            panic!("unexpected frames: {frames:?}");
        };
        assert_eq!(err.code(), ErrorCode::NotFound);
    }
}
