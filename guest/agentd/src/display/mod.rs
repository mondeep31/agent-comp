//! The guest's virtual screen.
//!
//! A guest booted with `ac.display=WxH` runs a second supervised child,
//! `agentd display WxH`, that keeps an X server and the image's desktop
//! session alive (see [`session`]). It publishes its state in
//! [`STATE_FILE`], which the server reports in `Pong.display` so the host
//! knows when the screen is usable.

#[cfg(target_os = "linux")]
pub mod session;

use std::fmt;
use std::path::Path;

/// The X display every desktop guest uses.
pub const DISPLAY: &str = ":0";
/// Written by the session supervisor, read by the server.
pub const STATE_FILE: &str = "/run/agent-comp/display";

/// Screen size from the kernel command line.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct DisplayConfig {
    pub width: u32,
    pub height: u32,
}

impl fmt::Display for DisplayConfig {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}x{}", self.width, self.height)
    }
}

impl DisplayConfig {
    /// Parses `WxH` within the sizes acd accepts.
    #[must_use]
    pub fn parse(s: &str) -> Option<Self> {
        let (w, h) = s.split_once('x')?;
        let width: u32 = w.parse().ok()?;
        let height: u32 = h.parse().ok()?;
        ((320..=7680).contains(&width) && (240..=4320).contains(&height))
            .then_some(Self { width, height })
    }

    /// Reads `ac.display=WxH` from a kernel command line.
    #[must_use]
    pub fn from_cmdline(cmdline: &str) -> Option<Self> {
        cmdline
            .split_whitespace()
            .find_map(|arg| arg.strip_prefix("ac.display="))
            .and_then(Self::parse)
    }
}

/// What the session supervisor last reported.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Status {
    pub ready: bool,
    pub error: String,
}

impl Status {
    /// The state file holds `ready` or `starting`, optionally followed by a
    /// newline and an error message.
    #[must_use]
    pub fn parse(s: &str) -> Self {
        let (head, error) = s.split_once('\n').unwrap_or((s, ""));
        Self {
            ready: head.trim() == "ready",
            error: error.trim().to_owned(),
        }
    }

    #[must_use]
    pub fn encode(&self) -> String {
        let head = if self.ready { "ready" } else { "starting" };
        if self.error.is_empty() {
            format!("{head}\n")
        } else {
            format!("{head}\n{}\n", self.error)
        }
    }

    /// Reads the state file; a missing file means the session has not started.
    #[must_use]
    pub fn read(path: &Path) -> Self {
        std::fs::read_to_string(path)
            .map(|s| Self::parse(&s))
            .unwrap_or_default()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_display_from_cmdline() {
        assert_eq!(
            DisplayConfig::from_cmdline("console=ttyS0 ac.display=1280x800 quiet"),
            Some(DisplayConfig {
                width: 1280,
                height: 800
            })
        );
        assert_eq!(DisplayConfig::from_cmdline("console=ttyS0"), None);
    }

    #[test]
    fn rejects_bad_sizes() {
        for bad in [
            "",
            "1280",
            "x800",
            "1280x",
            "0x0",
            "99999x800",
            "1280x800x24",
            "-1x5",
        ] {
            assert_eq!(DisplayConfig::parse(bad), None, "{bad:?}");
        }
    }

    #[test]
    fn status_round_trips() {
        for st in [
            Status::default(),
            Status {
                ready: true,
                error: String::new(),
            },
            Status {
                ready: false,
                error: "Xvfb exited: status 1".into(),
            },
        ] {
            assert_eq!(Status::parse(&st.encode()), st);
        }
        assert_eq!(
            Status::read(Path::new("/nonexistent/display")),
            Status::default()
        );
    }
}
