//! Screen capture and synthetic input over the X protocol (XTEST).
//!
//! One connection to `:0` is shared by all requests and reopened after a
//! failure. Calls block, so the server runs them on tokio's blocking pool.

use std::collections::HashSet;
use std::thread::sleep;
use std::time::Duration;

use x11rb::connection::Connection;
use x11rb::errors::{ConnectionError, ReplyError};
use x11rb::protocol::xproto::{ConnectionExt as _, ImageFormat, ImageOrder, Window};
use x11rb::protocol::xtest::ConnectionExt as _;
use x11rb::rust_connection::RustConnection;

use super::keys::{self, Keysym};
use crate::error::AgentError;
use crate::proto::ErrorCode;

const KEY_PRESS: u8 = 2;
const KEY_RELEASE: u8 = 3;
const BUTTON_PRESS: u8 = 4;
const BUTTON_RELEASE: u8 = 5;
const MOTION_NOTIFY: u8 = 6;

/// Wheel buttons: up, down, left, right.
const WHEEL_UP: u8 = 4;
const WHEEL_DOWN: u8 = 5;
const WHEEL_LEFT: u8 = 6;
const WHEEL_RIGHT: u8 = 7;

/// Lets other clients pick up a keyboard mapping change before keys from the
/// new mapping arrive.
const REMAP_SETTLE: Duration = Duration::from_millis(30);

/// An RGB screen capture.
#[derive(Debug)]
pub struct Rgb {
    pub width: u32,
    pub height: u32,
    pub data: Vec<u8>,
}

/// An open connection plus the keyboard state input needs.
pub struct Conn {
    x: RustConnection,
    root: Window,
    width: u16,
    height: u16,
    min_keycode: u8,
    max_keycode: u8,
    /// Keysyms per keycode in `keymap`.
    per: usize,
    keymap: Vec<Keysym>,
    /// Keycodes with no keysyms, borrowed to type characters the keymap lacks.
    scratch: Vec<u8>,
    /// Next scratch keycode to reuse (round robin).
    next_scratch: usize,
    /// Keys and buttons left pressed by an `ACTION_PRESS`, across batches.
    held_keys: HashSet<u8>,
    held_buttons: HashSet<u8>,
}

impl std::fmt::Debug for Conn {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Conn")
            .field("width", &self.width)
            .field("height", &self.height)
            .finish_non_exhaustive()
    }
}

fn conn_err(e: &ConnectionError) -> AgentError {
    AgentError::new(ErrorCode::Unavailable, format!("display connection: {e}"))
}

fn reply_err(e: &ReplyError) -> AgentError {
    match e {
        ReplyError::ConnectionError(c) => conn_err(c),
        ReplyError::X11Error(x) => AgentError::new(
            ErrorCode::Internal,
            format!("display: X error {:?}", x.error_kind),
        ),
    }
}

impl Conn {
    /// Connects to `display` (e.g. ":0").
    pub fn open(display: &str) -> Result<Self, AgentError> {
        let (conn, screen) = RustConnection::connect(Some(display)).map_err(|e| {
            AgentError::new(
                ErrorCode::Unavailable,
                format!("connecting to the display: {e}"),
            )
        })?;
        let setup = conn.setup();
        let s = setup
            .roots
            .get(screen)
            .ok_or_else(|| AgentError::new(ErrorCode::Unavailable, "display has no screen"))?;
        if s.root_depth != 24 || setup.image_byte_order != ImageOrder::LSB_FIRST {
            return Err(AgentError::new(
                ErrorCode::Unavailable,
                format!("unsupported screen: depth {}", s.root_depth),
            ));
        }
        let mut c = Self {
            root: s.root,
            width: s.width_in_pixels,
            height: s.height_in_pixels,
            min_keycode: setup.min_keycode,
            max_keycode: setup.max_keycode,
            x: conn,
            per: 0,
            keymap: Vec::new(),
            scratch: Vec::new(),
            next_scratch: 0,
            held_keys: HashSet::new(),
            held_buttons: HashSet::new(),
        };
        c.load_keymap()?;
        Ok(c)
    }

    #[must_use]
    pub fn size(&self) -> (u32, u32) {
        (u32::from(self.width), u32::from(self.height))
    }

    /// Captures a rectangle of the screen as RGB.
    pub fn capture(&self, x: u16, y: u16, w: u16, h: u16) -> Result<Rgb, AgentError> {
        let img = self
            .x
            .get_image(
                ImageFormat::Z_PIXMAP,
                self.root,
                x as i16,
                y as i16,
                w,
                h,
                !0,
            )
            .map_err(|e| conn_err(&e))?
            .reply()
            .map_err(|e| reply_err(&e))?;
        let pixels = usize::from(w) * usize::from(h);
        // Depth 24 is stored as 32-bit BGRX, rows padded to 32 bits (no padding).
        if img.data.len() < pixels * 4 {
            return Err(AgentError::new(
                ErrorCode::Internal,
                format!(
                    "display: short image ({} bytes for {w}x{h})",
                    img.data.len()
                ),
            ));
        }
        let mut data = Vec::with_capacity(pixels * 3);
        for px in img.data.as_chunks::<4>().0.iter().take(pixels) {
            data.extend_from_slice(&[px[2], px[1], px[0]]);
        }
        Ok(Rgb {
            width: u32::from(w),
            height: u32::from(h),
            data,
        })
    }

    /// The pointer's position.
    pub fn cursor(&self) -> Result<(i32, i32), AgentError> {
        let p = self
            .x
            .query_pointer(self.root)
            .map_err(|e| conn_err(&e))?
            .reply()
            .map_err(|e| reply_err(&e))?;
        Ok((i32::from(p.root_x), i32::from(p.root_y)))
    }

    /// Waits until the server has processed every request sent so far.
    pub fn sync(&self) -> Result<(), AgentError> {
        self.x
            .get_input_focus()
            .map_err(|e| conn_err(&e))?
            .reply()
            .map_err(|e| reply_err(&e))?;
        Ok(())
    }

    fn fake(&self, kind: u8, detail: u8, x: i16, y: i16) -> Result<(), AgentError> {
        self.x
            .xtest_fake_input(kind, detail, 0, self.root, x, y, 0)
            .map_err(|e| conn_err(&e))?;
        Ok(())
    }

    fn flush(&self) -> Result<(), AgentError> {
        self.x.flush().map_err(|e| conn_err(&e))
    }

    pub fn move_to(&self, x: i32, y: i32) -> Result<(), AgentError> {
        let x = x.clamp(0, i32::from(self.width) - 1) as i16;
        let y = y.clamp(0, i32::from(self.height) - 1) as i16;
        self.fake(MOTION_NOTIFY, 0, x, y)
    }

    pub fn button(&mut self, button: u8, press: bool) -> Result<(), AgentError> {
        if press {
            self.fake(BUTTON_PRESS, button, 0, 0)?;
            self.held_buttons.insert(button);
        } else {
            self.fake(BUTTON_RELEASE, button, 0, 0)?;
            self.held_buttons.remove(&button);
        }
        Ok(())
    }

    /// Wheel clicks; positive dy scrolls down, positive dx scrolls right.
    pub fn scroll(&mut self, dx: i32, dy: i32) -> Result<(), AgentError> {
        let steps = [
            (
                if dy < 0 { WHEEL_UP } else { WHEEL_DOWN },
                dy.unsigned_abs(),
            ),
            (
                if dx < 0 { WHEEL_LEFT } else { WHEEL_RIGHT },
                dx.unsigned_abs(),
            ),
        ];
        for (button, n) in steps {
            for _ in 0..n {
                self.button(button, true)?;
                self.button(button, false)?;
            }
        }
        Ok(())
    }

    pub fn key(&mut self, keycode: u8, press: bool) -> Result<(), AgentError> {
        if press {
            self.fake(KEY_PRESS, keycode, 0, 0)?;
            self.held_keys.insert(keycode);
        } else {
            self.fake(KEY_RELEASE, keycode, 0, 0)?;
            self.held_keys.remove(&keycode);
        }
        Ok(())
    }

    /// Releases every key and button this connection holds down.
    pub fn release_all(&mut self) {
        for k in std::mem::take(&mut self.held_keys) {
            let _ = self.fake(KEY_RELEASE, k, 0, 0);
        }
        for b in std::mem::take(&mut self.held_buttons) {
            let _ = self.fake(BUTTON_RELEASE, b, 0, 0);
        }
        let _ = self.flush();
    }

    #[must_use]
    pub fn held(&self) -> (HashSet<u8>, HashSet<u8>) {
        (self.held_keys.clone(), self.held_buttons.clone())
    }

    /// Releases keys and buttons pressed since `before` was taken.
    pub fn release_since(&mut self, before: &(HashSet<u8>, HashSet<u8>)) {
        let keys: Vec<u8> = self.held_keys.difference(&before.0).copied().collect();
        let buttons: Vec<u8> = self.held_buttons.difference(&before.1).copied().collect();
        for k in keys {
            let _ = self.key(k, false);
        }
        for b in buttons {
            let _ = self.button(b, false);
        }
        let _ = self.flush();
    }

    fn load_keymap(&mut self) -> Result<(), AgentError> {
        let count = self.max_keycode - self.min_keycode + 1;
        let reply = self
            .x
            .get_keyboard_mapping(self.min_keycode, count)
            .map_err(|e| conn_err(&e))?
            .reply()
            .map_err(|e| reply_err(&e))?;
        self.per = usize::from(reply.keysyms_per_keycode).max(1);
        self.keymap = reply.keysyms;
        self.scratch = (0..usize::from(count))
            .filter(|i| {
                self.keymap
                    .get(i * self.per..(i + 1) * self.per)
                    .is_some_and(|syms| syms.iter().all(|&k| k == 0))
            })
            .map(|i| self.min_keycode + i as u8)
            .collect();
        // Keep a few keycodes in reserve even on a full keymap.
        if self.scratch.is_empty() {
            self.scratch = vec![self.max_keycode, self.max_keycode - 1];
        }
        Ok(())
    }

    /// Finds the keycode for `sym` and whether it needs Shift.
    fn lookup(&self, sym: Keysym) -> Option<(u8, bool)> {
        self.keymap
            .chunks(self.per)
            .enumerate()
            .find_map(|(i, syms)| {
                syms.iter()
                    .take(2)
                    .position(|&k| k == sym)
                    .map(|col| (self.min_keycode + i as u8, col == 1))
            })
    }

    /// Makes every keysym in `syms` typeable, borrowing scratch keycodes for
    /// those the keymap lacks. Returns once other clients can see the change.
    pub fn ensure_keysyms(&mut self, syms: &[Keysym]) -> Result<(), AgentError> {
        let mut changed = false;
        for &sym in syms {
            if self.lookup(sym).is_some() {
                continue;
            }
            let kc = self.scratch[self.next_scratch % self.scratch.len()];
            self.next_scratch += 1;
            let mut row = vec![0; self.per];
            row[0] = sym;
            if self.per > 1 {
                row[1] = sym;
            }
            self.x
                .change_keyboard_mapping(1, kc, self.per as u8, &row)
                .map_err(|e| conn_err(&e))?;
            let at = usize::from(kc - self.min_keycode) * self.per;
            self.keymap[at..at + self.per].copy_from_slice(&row);
            changed = true;
        }
        if changed {
            self.sync()?;
            sleep(REMAP_SETTLE);
        }
        Ok(())
    }

    /// Presses or releases the key for `sym`, with Shift when the keymap
    /// needs it. Call `ensure_keysyms` first.
    pub fn keysym(&mut self, sym: Keysym, press: bool) -> Result<(), AgentError> {
        let (kc, shift) = self.lookup(sym).ok_or_else(|| {
            AgentError::new(
                ErrorCode::Internal,
                format!("keysym {sym:#x} is not mapped"),
            )
        })?;
        let shift_kc = if shift {
            self.lookup(keys::SHIFT_L).map(|(k, _)| k)
        } else {
            None
        };
        if press {
            if let Some(s) = shift_kc {
                self.key(s, true)?;
            }
            self.key(kc, true)?;
        } else {
            self.key(kc, false)?;
            if let Some(s) = shift_kc {
                self.key(s, false)?;
            }
        }
        Ok(())
    }

    /// Sends everything queued and waits for the server to apply it.
    pub fn commit(&self) -> Result<(), AgentError> {
        self.flush()?;
        self.sync()
    }

    pub fn flush_now(&self) -> Result<(), AgentError> {
        self.flush()
    }
}
