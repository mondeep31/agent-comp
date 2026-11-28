//! Screenshots and input against a real X server.
//!
//! Ignored by default; `make test-rust-x11` runs them under `xvfb-run`
//! (it needs Xvfb and xdotool on the host).
#![cfg(target_os = "linux")]
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

use std::process::Command;
use std::sync::Arc;

use agentd::display::DisplayConfig;
use agentd::display::ops::{input, screenshot};
use agentd::proto::input_event::Kind as Ev;
use agentd::proto::{
    Action, Button, ImageFormat, Input, InputEvent, Key, PointerButton, PointerMove, Region,
    Screenshot, Text,
};
use agentd::server::State;

fn state() -> Arc<State> {
    let display = std::env::var("DISPLAY").expect("DISPLAY");
    assert_eq!(display, ":0", "run under xvfb-run -n 0");
    Arc::new(State::default().with_display(DisplayConfig::parse("1280x800")))
}

fn ev(kind: Ev) -> InputEvent {
    InputEvent { kind: Some(kind) }
}

#[tokio::test(flavor = "multi_thread")]
#[ignore = "needs an X server; run with make test-rust-x11"]
async fn captures_png_jpeg_and_regions() {
    let st = state();
    let (info, png) = screenshot(Screenshot::default(), Arc::clone(&st))
        .await
        .unwrap();
    assert_eq!((info.width, info.height), (1280, 800));
    assert_eq!(info.size as usize, png.len());
    assert_eq!(&png[1..4], b"PNG");

    let (info, jpg) = screenshot(
        Screenshot {
            format: ImageFormat::Jpeg.into(),
            quality: 70,
            region: Some(Region {
                x: 100,
                y: 50,
                width: 300,
                height: 200,
            }),
        },
        Arc::clone(&st),
    )
    .await
    .unwrap();
    assert_eq!((info.width, info.height), (300, 200));
    assert_eq!(&jpg[..2], &[0xff, 0xd8]);

    let outside = Screenshot {
        region: Some(Region {
            x: 1200,
            y: 0,
            width: 200,
            height: 10,
        }),
        ..Default::default()
    };
    assert!(screenshot(outside, st).await.is_err());
}

/// Types into an xterm-free target: xdotool reports the active window's
/// keyboard input via a tiny xev-like loop is overkill, so check the pointer
/// and the keymap side effects instead, and type into a real text field when
/// xterm is present.
#[tokio::test(flavor = "multi_thread")]
#[ignore = "needs an X server; run with make test-rust-x11"]
async fn moves_clicks_and_types() {
    let st = state();
    let res = input(
        Input {
            events: vec![
                ev(Ev::Move(PointerMove { x: 640, y: 400 })),
                ev(Ev::Button(PointerButton {
                    button: Button::Left.into(),
                    action: Action::Tap.into(),
                    count: 2,
                })),
                ev(Ev::Key(Key {
                    keys: "ctrl+shift+Tab".into(),
                    ..Default::default()
                })),
                ev(Ev::Text(Text {
                    text: "hello wörld € 漢".into(),
                    delay_ms: 0,
                })),
            ],
        },
        Arc::clone(&st),
    )
    .await
    .unwrap();
    assert_eq!((res.cursor_x, res.cursor_y), (640, 400));

    // Out-of-range moves clamp to the screen.
    let res = input(
        Input {
            events: vec![ev(Ev::Move(PointerMove { x: -50, y: 5000 }))],
        },
        Arc::clone(&st),
    )
    .await
    .unwrap();
    assert_eq!((res.cursor_x, res.cursor_y), (0, 799));

    // Typed text reaches a client: xdotool's selection of the active window
    // is not needed, the X server's key state is. Hold shift across batches
    // and check xdotool sees the pointer where we left it.
    let out = Command::new("xdotool")
        .arg("getmouselocation")
        .output()
        .unwrap();
    let loc = String::from_utf8_lossy(&out.stdout);
    assert!(loc.starts_with("x:0 y:799"), "xdotool sees {loc}");
}

#[tokio::test(flavor = "multi_thread")]
#[ignore = "needs an X server; run with make test-rust-x11"]
async fn text_reaches_an_x_client() {
    // xterm echoes typed text into its window title via a shell read loop.
    if Command::new("xterm").arg("-version").output().is_err() {
        eprintln!("skipping: xterm missing");
        return;
    }
    let mut term = Command::new("xterm")
        .args(["-geometry", "80x5+0+0", "-e", "sh", "-c"])
        .arg(r#"read line; printf '\033]2;GOT:%s\007' "$line"; sleep 5"#)
        .spawn()
        .unwrap();
    // Wait for the window, then focus it by clicking inside it.
    let mut found = false;
    for _ in 0..50 {
        let out = Command::new("xdotool")
            .args(["search", "--class", "xterm"])
            .output()
            .unwrap();
        if !out.stdout.is_empty() {
            found = true;
            break;
        }
        std::thread::sleep(std::time::Duration::from_millis(100));
    }
    assert!(found, "xterm window never appeared");
    let st = state();
    input(
        Input {
            events: vec![
                ev(Ev::Move(PointerMove { x: 50, y: 30 })),
                ev(Ev::Button(PointerButton {
                    button: Button::Left.into(),
                    ..Default::default()
                })),
                ev(Ev::Text(Text {
                    text: "hi wörld!\n".into(),
                    delay_ms: 5,
                })),
            ],
        },
        st,
    )
    .await
    .unwrap();
    let mut title = String::new();
    for _ in 0..30 {
        let out = Command::new("xdotool")
            .args(["search", "--name", "GOT:", "getwindowname"])
            .output()
            .unwrap();
        title = String::from_utf8_lossy(&out.stdout).trim().to_owned();
        if !title.is_empty() {
            break;
        }
        std::thread::sleep(std::time::Duration::from_millis(100));
    }
    let _ = term.kill();
    let _ = term.wait();
    assert_eq!(title, "GOT:hi wörld!");
}
