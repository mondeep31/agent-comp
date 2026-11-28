# Desktop image: a virtual screen (Xvfb) with openbox and Chromium, for
# computer-use agents. Debian, because Ubuntu ships Chromium only as a snap.
# agentd runs the session from /etc/agent-comp/desktop when the guest boots
# with ac.display=WxH (acd adds it for images with a display).
# shellcheck shell=bash disable=SC2034
NAME="${NAME:-desktop}"
SUITE="${SUITE:-trixie}"
MIRROR="${MIRROR:-http://deb.debian.org/debian}"
COMPONENTS=main

PACKAGES=(
  # agentd and networking
  iproute2 iputils-ping ca-certificates netbase
  # a lean shell
  bash coreutils procps psmisc less file findutils grep sed gawk diffutils
  tar gzip xz-utils unzip jq nano curl git python3 sqlite3
  # the screen
  xvfb xkb-data x11-xkb-utils x11-utils xdotool openbox xterm
  chromium chromium-sandbox
  fonts-dejavu-core fonts-liberation
)

write_sources() {
  rm -f "$root/etc/apt/sources.list"
  cat >"$root/etc/apt/sources.list.d/debian.sources" <<X
Types: deb
URIs: $MIRROR
Suites: $SUITE $SUITE-updates
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg

Types: deb
URIs: http://security.debian.org/debian-security
Suites: $SUITE-security
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
X
}

post_install() {
  chroot "$root" useradd --create-home --uid 1000 --shell /bin/bash agent
  install -d -m 0755 "$root/etc/agent-comp/desktop"

  # The session: a window manager and a maximized browser. agentd restarts the
  # whole session if this script exits, so it waits on the browser.
  cat >"$root/etc/agent-comp/desktop/session" <<'X'
#!/bin/sh
openbox &
exec chromium \
  --no-first-run --no-default-browser-check --disable-background-networking \
  --disable-component-update --disable-sync --disable-features=Translate,MediaRouter \
  --disable-dev-shm-usage --disable-gpu --password-store=basic \
  --start-maximized --window-position=0,0 \
  --user-data-dir="$HOME/.config/chromium" about:blank
X
  # Ready once Chromium has mapped a window.
  cat >"$root/etc/agent-comp/desktop/ready" <<'X'
#!/bin/sh
exec xdotool search --onlyvisible --class chromium >/dev/null
X
  chmod 0755 "$root/etc/agent-comp/desktop/session" "$root/etc/agent-comp/desktop/ready"

  # Managed policy: no sign-in prompts, update checks or background chatter.
  install -d -m 0755 "$root/etc/chromium/policies/managed"
  cat >"$root/etc/chromium/policies/managed/agent-comp.json" <<'X'
{
  "BrowserSignin": 0,
  "SyncDisabled": true,
  "PromotionalTabsEnabled": false,
  "DefaultBrowserSettingEnabled": false,
  "MetricsReportingEnabled": false,
  "BackgroundModeEnabled": false,
  "PasswordManagerEnabled": false,
  "TranslateEnabled": false,
  "HomepageLocation": "about:blank",
  "RestoreOnStartup": 5
}
X

  # openbox: no decorations to click past, every window maximized.
  install -d -m 0755 "$root/home/agent/.config/openbox"
  cat >"$root/home/agent/.config/openbox/rc.xml" <<'X'
<?xml version="1.0" encoding="UTF-8"?>
<openbox_config xmlns="http://openbox.org/3.4/rc">
  <desktops><number>1</number></desktops>
  <applications>
    <application class="*"><decor>no</decor><maximized>yes</maximized></application>
  </applications>
</openbox_config>
X
  chroot "$root" chown -R agent:agent /home/agent
}
