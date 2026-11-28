//! The `Screenshot` and `Input` operations.

use std::io::Cursor;
use std::sync::Arc;
use std::thread::sleep;
use std::time::Duration;

use super::keys;
use super::x11::{Conn, Rgb};
use crate::error::AgentError;
use crate::proto::input_event::Kind as Ev;
use crate::proto::{
    Action, Button, ErrorCode, ImageFormat, Input, InputResult, Screenshot, ScreenshotInfo,
};
use crate::server::State;

/// Most events in one `Input`.
pub const MAX_EVENTS: usize = 64;
/// Most characters typed by one `Input`.
pub const MAX_TEXT_CHARS: usize = 4096;
/// Longest total wait (delays plus per-character pauses) in one `Input`.
pub const MAX_DELAY: Duration = Duration::from_secs(30);
/// Most taps in one button or key event.
pub const MAX_REPEAT: u32 = 100;
/// Largest encoded image.
pub const MAX_IMAGE_BYTES: usize = 8 << 20;
const DEFAULT_JPEG_QUALITY: u8 = 80;
/// Time between the taps of a double or triple click.
const CLICK_GAP: Duration = Duration::from_millis(40);

/// Runs `f` with the shared X connection, opening it on first use and
/// dropping it after a connection failure so the next call reconnects.
fn with_conn<T>(
    state: &State,
    f: impl FnOnce(&mut Conn) -> Result<T, AgentError>,
) -> Result<T, AgentError> {
    if state.display.is_none() {
        return Err(AgentError::new(
            ErrorCode::Unavailable,
            "this sandbox has no display",
        ));
    }
    let mut guard = state
        .x11
        .lock()
        .map_err(|_| AgentError::new(ErrorCode::Internal, "display lock poisoned"))?;
    if guard.is_none() {
        *guard = Some(Conn::open(super::DISPLAY)?);
    }
    let Some(conn) = guard.as_mut() else {
        return Err(AgentError::new(
            ErrorCode::Internal,
            "display connection missing",
        ));
    };
    let result = f(conn);
    if matches!(&result, Err(e) if e.code == ErrorCode::Unavailable) {
        *guard = None;
    }
    result
}

async fn blocking<T: Send + 'static>(
    f: impl FnOnce() -> Result<T, AgentError> + Send + 'static,
) -> Result<T, AgentError> {
    tokio::task::spawn_blocking(f)
        .await
        .map_err(|e| AgentError::new(ErrorCode::Internal, format!("display task: {e}")))?
}

/// Captures and encodes the screen.
pub async fn screenshot(
    req: Screenshot,
    state: Arc<State>,
) -> Result<(ScreenshotInfo, Vec<u8>), AgentError> {
    let format = match req.format() {
        ImageFormat::Unspecified | ImageFormat::Png => ImageFormat::Png,
        ImageFormat::Jpeg => ImageFormat::Jpeg,
    };
    let quality = match req.quality {
        0 => DEFAULT_JPEG_QUALITY,
        q @ 1..=100 => q as u8,
        q => return Err(AgentError::invalid(format!("quality {q} is not in 1-100"))),
    };
    let region = req.region;
    let rgb = blocking(move || {
        with_conn(&state, |c| {
            let (w, h) = c.size();
            let (x, y, rw, rh) = match region {
                None => (0, 0, w, h),
                Some(r) => {
                    if r.width == 0
                        || r.height == 0
                        || r.x.saturating_add(r.width) > w
                        || r.y.saturating_add(r.height) > h
                    {
                        return Err(AgentError::invalid(format!(
                            "region {}x{}+{}+{} is outside the {w}x{h} screen",
                            r.width, r.height, r.x, r.y
                        )));
                    }
                    (r.x, r.y, r.width, r.height)
                }
            };
            c.capture(x as u16, y as u16, rw as u16, rh as u16)
        })
    })
    .await?;
    let data = encode(&rgb, format, quality)?;
    if data.len() > MAX_IMAGE_BYTES {
        return Err(AgentError::new(
            ErrorCode::TooLarge,
            format!("encoded image is {} bytes", data.len()),
        ));
    }
    Ok((
        ScreenshotInfo {
            width: rgb.width,
            height: rgb.height,
            format: format.into(),
            size: data.len() as u64,
        },
        data,
    ))
}

/// Encodes RGB pixels as PNG or JPEG.
pub fn encode(rgb: &Rgb, format: ImageFormat, quality: u8) -> Result<Vec<u8>, AgentError> {
    let internal = |e: String| AgentError::new(ErrorCode::Internal, format!("encoding: {e}"));
    let mut out = Vec::with_capacity(rgb.data.len() / 8);
    match format {
        ImageFormat::Jpeg => {
            let (w, h) = (
                u16::try_from(rgb.width).map_err(|e| internal(e.to_string()))?,
                u16::try_from(rgb.height).map_err(|e| internal(e.to_string()))?,
            );
            jpeg_encoder::Encoder::new(&mut out, quality)
                .encode(&rgb.data, w, h, jpeg_encoder::ColorType::Rgb)
                .map_err(|e| internal(e.to_string()))?;
        }
        ImageFormat::Png | ImageFormat::Unspecified => {
            let mut enc = png::Encoder::new(Cursor::new(&mut out), rgb.width, rgb.height);
            enc.set_color(png::ColorType::Rgb);
            enc.set_depth(png::BitDepth::Eight);
            enc.set_compression(png::Compression::Fast);
            let mut w = enc.write_header().map_err(|e| internal(e.to_string()))?;
            w.write_image_data(&rgb.data)
                .map_err(|e| internal(e.to_string()))?;
            w.finish().map_err(|e| internal(e.to_string()))?;
        }
    }
    Ok(out)
}

/// Checks the caps before touching the display.
pub fn validate(req: &Input) -> Result<(), AgentError> {
    if req.events.len() > MAX_EVENTS {
        return Err(AgentError::invalid(format!(
            "at most {MAX_EVENTS} events per input"
        )));
    }
    let mut chars = 0usize;
    let mut wait = Duration::ZERO;
    for ev in &req.events {
        match &ev.kind {
            None => return Err(AgentError::invalid("empty input event")),
            Some(Ev::Text(t)) => {
                let n = t.text.chars().count();
                chars += n;
                wait += Duration::from_millis(u64::from(t.delay_ms)) * n as u32;
            }
            Some(Ev::Delay(d)) => wait += Duration::from_millis(u64::from(d.ms)),
            Some(Ev::Key(k)) => {
                keys::chord(&k.keys).map_err(AgentError::invalid)?;
                if k.count > MAX_REPEAT {
                    return Err(AgentError::invalid(format!(
                        "count is at most {MAX_REPEAT}"
                    )));
                }
            }
            Some(Ev::Button(b)) => {
                if b.button() == Button::Unspecified {
                    return Err(AgentError::invalid("button is required"));
                }
                if b.count > MAX_REPEAT {
                    return Err(AgentError::invalid(format!(
                        "count is at most {MAX_REPEAT}"
                    )));
                }
            }
            Some(Ev::Scroll(s)) => {
                if s.dx.unsigned_abs() > MAX_REPEAT || s.dy.unsigned_abs() > MAX_REPEAT {
                    return Err(AgentError::invalid(format!(
                        "scroll is at most {MAX_REPEAT} clicks"
                    )));
                }
            }
            Some(Ev::Move(_)) => {}
        }
    }
    if chars > MAX_TEXT_CHARS {
        return Err(AgentError::invalid(format!(
            "at most {MAX_TEXT_CHARS} characters of text per input"
        )));
    }
    if wait > MAX_DELAY {
        return Err(AgentError::invalid(format!(
            "at most {}s of delay per input",
            MAX_DELAY.as_secs()
        )));
    }
    Ok(())
}

/// Applies a batch of input events and reports where the pointer ended up.
pub async fn input(req: Input, state: Arc<State>) -> Result<InputResult, AgentError> {
    validate(&req)?;
    blocking(move || {
        with_conn(&state, |c| {
            let before = c.held();
            let applied = req.events.iter().try_for_each(|ev| apply(c, ev));
            if let Err(e) = applied {
                c.release_since(&before);
                return Err(e);
            }
            c.commit()?;
            let (x, y) = c.cursor()?;
            Ok(InputResult {
                cursor_x: x,
                cursor_y: y,
            })
        })
    })
    .await
}

fn x_button(b: Button) -> u8 {
    match b {
        Button::Unspecified | Button::Left => 1,
        Button::Middle => 2,
        Button::Right => 3,
    }
}

fn apply(c: &mut Conn, ev: &crate::proto::InputEvent) -> Result<(), AgentError> {
    match &ev.kind {
        None => Err(AgentError::invalid("empty input event")),
        Some(Ev::Move(m)) => {
            c.move_to(m.x, m.y)?;
            c.flush_now()
        }
        Some(Ev::Button(b)) => {
            let button = x_button(b.button());
            match b.action() {
                Action::Press => c.button(button, true)?,
                Action::Release => c.button(button, false)?,
                Action::Unspecified | Action::Tap => {
                    for i in 0..b.count.max(1) {
                        if i > 0 {
                            c.flush_now()?;
                            sleep(CLICK_GAP);
                        }
                        c.button(button, true)?;
                        c.button(button, false)?;
                    }
                }
            }
            c.flush_now()
        }
        Some(Ev::Scroll(s)) => {
            c.scroll(s.dx, s.dy)?;
            c.flush_now()
        }
        Some(Ev::Key(k)) => {
            let syms = keys::chord(&k.keys).map_err(AgentError::invalid)?;
            c.ensure_keysyms(&syms)?;
            let press = |c: &mut Conn| syms.iter().try_for_each(|&s| c.keysym(s, true));
            let release = |c: &mut Conn| syms.iter().rev().try_for_each(|&s| c.keysym(s, false));
            match k.action() {
                Action::Press => press(c)?,
                Action::Release => release(c)?,
                Action::Unspecified | Action::Tap => {
                    for _ in 0..k.count.max(1) {
                        press(c)?;
                        release(c)?;
                    }
                }
            }
            c.flush_now()
        }
        Some(Ev::Text(t)) => {
            let pause = Duration::from_millis(u64::from(t.delay_ms));
            for ch in t.text.chars() {
                let sym = keys::char_keysym(ch);
                c.ensure_keysyms(&[sym])?;
                c.keysym(sym, true)?;
                c.keysym(sym, false)?;
                // Let clients read a borrowed keycode before it is remapped.
                c.commit()?;
                if !pause.is_zero() {
                    sleep(pause);
                }
            }
            Ok(())
        }
        Some(Ev::Delay(d)) => {
            c.flush_now()?;
            sleep(Duration::from_millis(u64::from(d.ms)));
            Ok(())
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::proto::{Delay, InputEvent, Key, PointerButton, Scroll, Text};

    fn ev(kind: Ev) -> InputEvent {
        InputEvent { kind: Some(kind) }
    }

    #[test]
    fn png_and_jpeg_round_trip() {
        let rgb = Rgb {
            width: 4,
            height: 2,
            data: (0..24).map(|i| (i * 10) as u8).collect(),
        };
        let png_bytes = encode(&rgb, ImageFormat::Png, 80).unwrap();
        let dec = png::Decoder::new(Cursor::new(&png_bytes));
        let mut reader = dec.read_info().unwrap();
        let mut buf = vec![0; reader.output_buffer_size()];
        let info = reader.next_frame(&mut buf).unwrap();
        assert_eq!((info.width, info.height), (4, 2));
        assert_eq!(&buf[..info.buffer_size()], rgb.data.as_slice());

        let jpg = encode(&rgb, ImageFormat::Jpeg, 75).unwrap();
        assert_eq!(&jpg[..2], &[0xff, 0xd8]);
    }

    #[test]
    fn caps_are_enforced() {
        let many = Input {
            events: (0..=MAX_EVENTS)
                .map(|_| ev(Ev::Delay(Delay { ms: 1 })))
                .collect(),
        };
        assert!(validate(&many).is_err());

        let long_text = Input {
            events: vec![ev(Ev::Text(Text {
                text: "x".repeat(MAX_TEXT_CHARS + 1),
                delay_ms: 0,
            }))],
        };
        assert!(validate(&long_text).is_err());

        let slow = Input {
            events: vec![ev(Ev::Text(Text {
                text: "x".repeat(100),
                delay_ms: 400,
            }))],
        };
        assert!(validate(&slow).is_err());

        let bad_key = Input {
            events: vec![ev(Ev::Key(Key {
                keys: "ctrl+nosuchkey".into(),
                ..Default::default()
            }))],
        };
        assert!(validate(&bad_key).is_err());

        let no_button = Input {
            events: vec![ev(Ev::Button(PointerButton::default()))],
        };
        assert!(validate(&no_button).is_err());

        let big_scroll = Input {
            events: vec![ev(Ev::Scroll(Scroll { dx: 0, dy: -1000 }))],
        };
        assert!(validate(&big_scroll).is_err());

        let ok = Input {
            events: vec![
                ev(Ev::Key(Key {
                    keys: "ctrl+l".into(),
                    ..Default::default()
                })),
                ev(Ev::Text(Text {
                    text: "hello wörld".into(),
                    delay_ms: 0,
                })),
                ev(Ev::Delay(Delay { ms: 100 })),
            ],
        };
        assert!(validate(&ok).is_ok());
    }

    #[tokio::test]
    async fn headless_guest_is_unavailable() {
        let state = Arc::new(State::default());
        let err = screenshot(Screenshot::default(), Arc::clone(&state))
            .await
            .unwrap_err();
        assert_eq!(err.code, ErrorCode::Unavailable);
        let err = input(Input::default(), state).await.unwrap_err();
        assert_eq!(err.code, ErrorCode::Unavailable);
    }
}
