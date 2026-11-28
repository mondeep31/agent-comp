//! Length-prefixed protobuf framing: a big-endian `u32` length, then a `Frame`.

use bytes::BytesMut;
use prost::Message;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};

use crate::proto::{Frame, frame::Kind};

/// Largest frame either side accepts.
pub const MAX_FRAME_BYTES: usize = 4 << 20;

#[derive(Debug, thiserror::Error)]
pub enum CodecError {
    #[error("i/o: {0}")]
    Io(#[from] std::io::Error),
    #[error("frame of {0} bytes exceeds the {MAX_FRAME_BYTES} byte limit")]
    TooLarge(usize),
    #[error("decoding frame: {0}")]
    Decode(#[from] prost::DecodeError),
    #[error("frame has no kind")]
    Empty,
}

/// Reads one frame. Returns `Ok(None)` on a clean EOF before any length bytes.
pub async fn read_frame<R: AsyncRead + Unpin>(r: &mut R) -> Result<Option<Kind>, CodecError> {
    let mut len_buf = [0u8; 4];
    match r.read_exact(&mut len_buf).await {
        Ok(_) => {}
        Err(e) if e.kind() == std::io::ErrorKind::UnexpectedEof => return Ok(None),
        Err(e) => return Err(e.into()),
    }
    let len = u32::from_be_bytes(len_buf) as usize;
    if len > MAX_FRAME_BYTES {
        return Err(CodecError::TooLarge(len));
    }
    let mut buf = BytesMut::zeroed(len);
    r.read_exact(&mut buf).await?;
    let frame = Frame::decode(buf.freeze())?;
    frame.kind.map(Some).ok_or(CodecError::Empty)
}

/// Writes one frame and flushes.
pub async fn write_frame<W: AsyncWrite + Unpin>(w: &mut W, kind: Kind) -> Result<(), CodecError> {
    let frame = Frame { kind: Some(kind) };
    let len = frame.encoded_len();
    if len > MAX_FRAME_BYTES {
        return Err(CodecError::TooLarge(len));
    }
    let mut buf = Vec::with_capacity(4 + len);
    buf.extend_from_slice(&(len as u32).to_be_bytes());
    frame
        .encode(&mut buf)
        .map_err(|e| std::io::Error::other(e.to_string()))?;
    w.write_all(&buf).await?;
    w.flush().await?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::proto::{Ping, Pong};

    #[tokio::test]
    async fn round_trip_preserves_frame() {
        let (mut a, mut b) = tokio::io::duplex(1024);
        let sent = Kind::Pong(Pong {
            agent_version: "1.2.3".into(),
            uptime_ms: 42,
            display: None,
        });
        write_frame(&mut a, sent.clone()).await.unwrap();
        let got = read_frame(&mut b).await.unwrap();
        assert_eq!(got, Some(sent));
    }

    #[tokio::test]
    async fn clean_eof_returns_none() {
        let (a, mut b) = tokio::io::duplex(64);
        drop(a);
        assert_eq!(read_frame(&mut b).await.unwrap(), None);
    }

    #[tokio::test]
    async fn oversized_length_is_rejected() {
        let (mut a, mut b) = tokio::io::duplex(64);
        a.write_all(&u32::MAX.to_be_bytes()).await.unwrap();
        assert!(matches!(
            read_frame(&mut b).await,
            Err(CodecError::TooLarge(_))
        ));
    }

    #[tokio::test]
    async fn empty_message_kind_is_encoded() {
        let (mut a, mut b) = tokio::io::duplex(64);
        write_frame(&mut a, Kind::Ping(Ping {})).await.unwrap();
        assert_eq!(read_frame(&mut b).await.unwrap(), Some(Kind::Ping(Ping {})));
    }
}
