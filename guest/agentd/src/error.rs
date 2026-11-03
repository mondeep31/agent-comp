//! Mapping from local failures to protocol `Error` frames.

use std::io;

use crate::codec::CodecError;
use crate::proto::{self, ErrorCode};

/// A failure that is reported to the host as an `Error` frame.
#[derive(Debug, thiserror::Error)]
#[error("{message}")]
pub struct AgentError {
    pub code: ErrorCode,
    pub message: String,
}

impl AgentError {
    pub fn new(code: ErrorCode, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
        }
    }

    #[must_use]
    pub fn invalid(message: impl Into<String>) -> Self {
        Self::new(ErrorCode::InvalidArgument, message)
    }

    /// Wraps an I/O error with context, keeping a precise error code.
    #[must_use]
    pub fn io(context: &str, err: &io::Error) -> Self {
        let code = match err.kind() {
            io::ErrorKind::NotFound => ErrorCode::NotFound,
            io::ErrorKind::PermissionDenied => ErrorCode::PermissionDenied,
            io::ErrorKind::AlreadyExists => ErrorCode::AlreadyExists,
            io::ErrorKind::InvalidInput
            | io::ErrorKind::IsADirectory
            | io::ErrorKind::NotADirectory => ErrorCode::InvalidArgument,
            _ => ErrorCode::Internal,
        };
        Self::new(code, format!("{context}: {err}"))
    }

    #[must_use]
    pub fn to_proto(&self) -> proto::Error {
        proto::Error {
            code: self.code.into(),
            message: self.message.clone(),
        }
    }
}

impl From<CodecError> for AgentError {
    fn from(err: CodecError) -> Self {
        match err {
            CodecError::TooLarge(_) => Self::new(ErrorCode::TooLarge, err.to_string()),
            CodecError::Io(_) => Self::new(ErrorCode::Internal, err.to_string()),
            CodecError::Decode(_) | CodecError::Empty => Self::invalid(err.to_string()),
        }
    }
}
