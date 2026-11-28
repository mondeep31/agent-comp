//! Key names to X keysyms, and chord parsing ("ctrl+shift+Tab").
//!
//! Names follow X keysym names (as xdotool uses them), case-insensitively,
//! plus a few common aliases ("enter", "esc", "cmd", "pagedown"). A single
//! character names its own keysym.

/// An X keysym.
pub type Keysym = u32;

pub const SHIFT_L: Keysym = 0xffe1;
pub const RETURN: Keysym = 0xff0d;
pub const TAB: Keysym = 0xff09;

/// Longest chord accepted, modifiers included.
const MAX_CHORD: usize = 8;

const NAMED: &[(&str, Keysym)] = &[
    // Editing and navigation.
    ("backspace", 0xff08),
    ("tab", TAB),
    ("return", RETURN),
    ("enter", RETURN),
    ("escape", 0xff1b),
    ("esc", 0xff1b),
    ("delete", 0xffff),
    ("del", 0xffff),
    ("insert", 0xff63),
    ("home", 0xff50),
    ("end", 0xff57),
    ("left", 0xff51),
    ("up", 0xff52),
    ("right", 0xff53),
    ("down", 0xff54),
    ("page_up", 0xff55),
    ("pageup", 0xff55),
    ("prior", 0xff55),
    ("page_down", 0xff56),
    ("pagedown", 0xff56),
    ("next", 0xff56),
    ("space", 0x20),
    ("menu", 0xff67),
    ("print", 0xff61),
    ("pause", 0xff13),
    ("scroll_lock", 0xff14),
    ("num_lock", 0xff7f),
    ("caps_lock", 0xffe5),
    // Modifiers.
    ("shift", SHIFT_L),
    ("shift_l", SHIFT_L),
    ("shift_r", 0xffe2),
    ("ctrl", 0xffe3),
    ("control", 0xffe3),
    ("control_l", 0xffe3),
    ("control_r", 0xffe4),
    ("alt", 0xffe9),
    ("alt_l", 0xffe9),
    ("alt_r", 0xffea),
    ("meta", 0xffe7),
    ("meta_l", 0xffe7),
    ("super", 0xffeb),
    ("super_l", 0xffeb),
    ("super_r", 0xffec),
    ("cmd", 0xffeb),
    ("win", 0xffeb),
    // Keypad.
    ("kp_enter", 0xff8d),
    ("kp_add", 0xffab),
    ("kp_subtract", 0xffad),
    ("kp_multiply", 0xffaa),
    ("kp_divide", 0xffaf),
    ("kp_decimal", 0xffae),
    // Punctuation by X name.
    ("minus", 0x2d),
    ("plus", 0x2b),
    ("equal", 0x3d),
    ("comma", 0x2c),
    ("period", 0x2e),
    ("slash", 0x2f),
    ("backslash", 0x5c),
    ("semicolon", 0x3b),
    ("colon", 0x3a),
    ("apostrophe", 0x27),
    ("quotedbl", 0x22),
    ("grave", 0x60),
    ("bracketleft", 0x5b),
    ("bracketright", 0x5d),
    ("braceleft", 0x7b),
    ("braceright", 0x7d),
    ("parenleft", 0x28),
    ("parenright", 0x29),
    ("less", 0x3c),
    ("greater", 0x3e),
    ("question", 0x3f),
    ("exclam", 0x21),
    ("at", 0x40),
    ("numbersign", 0x23),
    ("dollar", 0x24),
    ("percent", 0x25),
    ("asciicircum", 0x5e),
    ("ampersand", 0x26),
    ("asterisk", 0x2a),
    ("underscore", 0x5f),
    ("bar", 0x7c),
    ("asciitilde", 0x7e),
    // Media keys.
    ("xf86audiolowervolume", 0x1008_ff11),
    ("xf86audiomute", 0x1008_ff12),
    ("xf86audioraisevolume", 0x1008_ff13),
];

/// The keysym that types `c`.
#[must_use]
pub fn char_keysym(c: char) -> Keysym {
    match c {
        '\n' | '\r' => RETURN,
        '\t' => TAB,
        // Latin-1 keysyms equal their code points.
        ' '..='~' | '\u{a0}'..='\u{ff}' => c as Keysym,
        _ => 0x0100_0000 | c as Keysym,
    }
}

/// Resolves one key name.
#[must_use]
pub fn keysym(name: &str) -> Option<Keysym> {
    let mut chars = name.chars();
    if let (Some(c), None) = (chars.next(), chars.next()) {
        return (!c.is_control()).then(|| char_keysym(c));
    }
    let lower = name.to_ascii_lowercase();
    if let Some(&(_, k)) = NAMED.iter().find(|(n, _)| *n == lower) {
        return Some(k);
    }
    // F1..F35 and KP_0..KP_9.
    if let Some(n) = lower.strip_prefix('f').and_then(|n| n.parse::<u32>().ok())
        && (1..=35).contains(&n)
    {
        return Some(0xffbe + n - 1);
    }
    if let Some(n) = lower
        .strip_prefix("kp_")
        .and_then(|n| n.parse::<u32>().ok())
        && n <= 9
    {
        return Some(0xffb0 + n);
    }
    None
}

/// Parses a chord such as "ctrl+shift+Tab" or "ctrl++" into keysyms, in
/// press order.
pub fn chord(spec: &str) -> Result<Vec<Keysym>, String> {
    let spec = spec.trim();
    if spec.is_empty() {
        return Err("empty key".into());
    }
    let mut parts: Vec<&str> = spec.split('+').collect();
    // A trailing "+" key leaves two empty parts: "ctrl++" -> ["ctrl", "", ""].
    if parts.len() >= 2 && parts[parts.len() - 1].is_empty() && parts[parts.len() - 2].is_empty() {
        parts.truncate(parts.len() - 2);
        parts.push("+");
    }
    if parts.len() > MAX_CHORD {
        return Err(format!("chord {spec:?} has more than {MAX_CHORD} keys"));
    }
    parts
        .iter()
        .map(|p| {
            let p = p.trim();
            keysym(p).ok_or_else(|| format!("unknown key {p:?} in {spec:?}"))
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn names_and_characters() {
        let cases: &[(&str, Keysym)] = &[
            ("Return", RETURN),
            ("enter", RETURN),
            ("ESC", 0xff1b),
            ("a", 0x61),
            ("A", 0x41),
            ("/", 0x2f),
            ("F1", 0xffbe),
            ("f12", 0xffc9),
            ("KP_7", 0xffb7),
            ("Page_Down", 0xff56),
            ("cmd", 0xffeb),
            ("€", 0x0100_20ac),
            ("é", 0xe9),
        ];
        for (name, want) in cases {
            assert_eq!(keysym(name), Some(*want), "{name}");
        }
        for bad in ["", "F0", "F36", "nosuchkey", "KP_10", "\u{1}"] {
            assert_eq!(keysym(bad), None, "{bad:?}");
        }
    }

    #[test]
    fn chords() {
        assert_eq!(chord("ctrl+l"), Ok(vec![0xffe3, 0x6c]));
        assert_eq!(chord("ctrl+shift+Tab"), Ok(vec![0xffe3, SHIFT_L, TAB]));
        assert_eq!(chord("ctrl++"), Ok(vec![0xffe3, 0x2b]));
        assert_eq!(chord("+"), Ok(vec![0x2b]));
        assert_eq!(chord(" alt + F4 "), Ok(vec![0xffe9, 0xffc1]));
        assert!(chord("").is_err());
        assert!(chord("ctrl+nosuchkey").is_err());
        assert!(chord("a+b+c+d+e+f+g+h+i").is_err());
    }

    #[test]
    fn text_characters() {
        assert_eq!(char_keysym('\n'), RETURN);
        assert_eq!(char_keysym('\t'), TAB);
        assert_eq!(char_keysym('x'), 0x78);
        assert_eq!(char_keysym('ö'), 0xf6);
        assert_eq!(char_keysym('漢'), 0x0100_6f22);
    }
}
