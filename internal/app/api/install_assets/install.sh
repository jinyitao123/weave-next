#!/bin/sh
set -eu

usage() {
  echo "Usage: install.sh --server <url> (--token <rtk_...> | --token-env <env-name> | --device-code <code>) [--dry-run]" >&2
  exit 1
}

SERVER=
TOKEN=
TOKEN_ENV=
DEVICE_CODE=
DRY_RUN=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --server) [ "$#" -ge 2 ] || usage; SERVER=$2; shift 2 ;;
    --token) [ "$#" -ge 2 ] || usage; TOKEN=$2; shift 2 ;;
    --token-env) [ "$#" -ge 2 ] || usage; TOKEN_ENV=$2; shift 2 ;;
    --device-code) [ "$#" -ge 2 ] || usage; DEVICE_CODE=$2; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    *) usage ;;
  esac
done
[ -n "$SERVER" ] || usage

# download URL DEST fetches a file and refuses it unless its SHA-256 matches
# the X-Weave-SHA256 header. A server too old to send the header is accepted
# for runtime installs only; device installs require it.
download() {
  HEADERS=$(mktemp)
  if ! curl -fsSL --retry 3 --retry-all-errors -D "$HEADERS" "$1" -o "$2"; then
    rm -f "$HEADERS"
    echo "Download failed: $1" >&2
    return 1
  fi
  EXPECTED=$(tr -d '\r' < "$HEADERS" | awk -F': *' 'tolower($1)=="x-weave-sha256"{v=$2} END{print v}')
  rm -f "$HEADERS"
  if [ -z "$EXPECTED" ]; then
    [ -z "$DEVICE_CODE" ] && return 0
    echo "The server did not send a checksum for $1; nothing was installed." >&2
    return 1
  fi
  if command -v shasum >/dev/null 2>&1; then
    ACTUAL=$(shasum -a 256 "$2" | awk '{print $1}')
  else
    ACTUAL=$(sha256sum "$2" | awk '{print $1}')
  fi
  if [ "$ACTUAL" != "$EXPECTED" ]; then
    echo "Checksum mismatch for $1; nothing was installed." >&2
    return 1
  fi
}

# install_device enrolls this Mac as the person's own device: the Weave node
# and the cua-driver release the server pins, both started at login, bound to
# the account that issued the code (weave-workbench decision 009).
install_device() {
  [ "$(uname -s)" = Darwin ] || { echo "Devices are Macs or Windows PCs; this machine is $(uname -s)." >&2; exit 1; }
  case "$(uname -m)" in
    x86_64) ARCH=amd64 ;;
    arm64) ARCH=arm64 ;;
    *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
  esac
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "Dry run: would install the Weave device node and cua-driver; no code was exchanged."
    return 0
  fi
  WEAVE_DIR="$HOME/.weave"
  BIN="$WEAVE_DIR/bin/weave-runtime"
  ENV_FILE="$WEAVE_DIR/device.env"
  LOG_FILE="$WEAVE_DIR/device.log"
  DRIVER_DIR="$WEAVE_DIR/cua-driver"
  mkdir -p "$WEAVE_DIR/bin" "$DRIVER_DIR"
  WORK=$(mktemp -d)
  trap 'rm -rf "$WORK"' EXIT HUP INT TERM

  echo "Downloading the Weave node and cua-driver..."
  download "$SERVER/v1/downloads/runtime/darwin/$ARCH" "$WORK/weave-runtime" || exit 1
  download "$SERVER/v1/downloads/cua-driver/darwin/$ARCH" "$WORK/cua-driver.tar.gz" || exit 1
  download "$SERVER/v1/downloads/cua-driver/license/notice" "$WORK/LICENSE" || exit 1
  mkdir "$WORK/driver"
  tar -xzf "$WORK/cua-driver.tar.gz" -C "$WORK/driver"
  NEW_APP=$(find "$WORK/driver" -maxdepth 3 -type d -name CuaDriver.app | head -n 1)
  [ -n "$NEW_APP" ] || { echo "The cua-driver archive has no CuaDriver.app; nothing was installed." >&2; exit 1; }

  # Exchange the code before touching the machine, so a wrong or used code
  # leaves nothing behind.
  PREVIOUS_ID=
  if [ -f "$ENV_FILE" ]; then
    PREVIOUS_ID=$(awk -F= '$1=="export WEAVE_RUNTIME_ID"{print $2}' "$ENV_FILE")
  fi
  HOST=$(scutil --get ComputerName 2>/dev/null || hostname)
  json_string() { printf '"%s"' "$(printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g')"; }
  printf '{"code":%s,"hostname":%s,"platform":"darwin","arch":"%s","runtime_id":%s}' \
    "$(json_string "$DEVICE_CODE")" "$(json_string "$HOST")" "$ARCH" "$(json_string "$PREVIOUS_ID")" > "$WORK/request.json"
  STATUS=$(curl -sS -o "$WORK/reply.json" -w '%{http_code}' -H 'Content-Type: application/json' \
    --data-binary "@$WORK/request.json" "$SERVER/v1/runtime/device-enrollments") || { echo "Could not reach $SERVER." >&2; exit 1; }
  if [ "$STATUS" != 200 ]; then
    MESSAGE=$(plutil -extract error raw -o - "$WORK/reply.json" 2>/dev/null || echo "HTTP $STATUS")
    echo "Registration failed: $MESSAGE" >&2
    exit 1
  fi
  RUNTIME_ID=$(plutil -extract runtime_id raw -o - "$WORK/reply.json")
  RUNTIME_TOKEN=$(plutil -extract token raw -o - "$WORK/reply.json")
  NAME=$(plutil -extract name raw -o - "$WORK/reply.json")

  APP_DIR=/Applications
  [ -w "$APP_DIR" ] || { APP_DIR="$HOME/Applications"; mkdir -p "$APP_DIR"; }
  APP="$APP_DIR/CuaDriver.app"
  DRIVER="$APP/Contents/MacOS/cua-driver"
  [ -x "$DRIVER" ] && "$DRIVER" stop >/dev/null 2>&1 || true
  rm -rf "$APP"
  ditto "$NEW_APP" "$APP"
  cp "$WORK/LICENSE" "$DRIVER_DIR/LICENSE"
  chmod 755 "$WORK/weave-runtime"
  mv -f "$WORK/weave-runtime" "$BIN"

  umask 077
  {
    printf 'export WEAVE_SERVER=%s\n' "$SERVER"
    printf 'export WEAVE_RUNTIME_TOKEN=%s\n' "$RUNTIME_TOKEN"
    printf 'export WEAVE_RUNTIME_ID=%s\n' "$RUNTIME_ID"
    printf 'export WEAVE_CUA_DRIVER=%s\n' "$DRIVER"
  } > "$ENV_FILE"
  chmod 600 "$ENV_FILE"

  PLIST="$HOME/Library/LaunchAgents/com.weave.device.plist"
  mkdir -p "$HOME/Library/LaunchAgents"
  cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>com.weave.device</string>
<key>ProgramArguments</key><array><string>/bin/sh</string><string>-c</string><string>. &quot;$ENV_FILE&quot; &amp;&amp; exec &quot;$BIN&quot; runtime --server &quot;\$WEAVE_SERVER&quot; --runtime-token &quot;\$WEAVE_RUNTIME_TOKEN&quot; --device --cua-driver &quot;\$WEAVE_CUA_DRIVER&quot;</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
<key>StandardOutPath</key><string>$LOG_FILE</string>
<key>StandardErrorPath</key><string>$LOG_FILE</string>
</dict></plist>
EOF
  # Into the person's login session even when this runs over ssh: the screen
  # can only be seen and driven from there.
  DOMAIN="gui/$(id -u)"
  launchctl bootout "$DOMAIN/com.weave.device" 2>/dev/null || true
  launchctl bootstrap "$DOMAIN" "$PLIST" 2>/dev/null || launchctl load "$PLIST"

  open -n -g -a "$APP" --args serve
  ( "$DRIVER" permissions grant >/dev/null 2>&1 & )

  echo "设备已注册：$NAME"
  echo "最后一步：在这台 Mac 的“系统设置 > 隐私与安全性”中，为 CuaDriver 打开“辅助功能”和“屏幕与系统录音”。"
  echo "日志：$LOG_FILE"
}

if [ -n "$DEVICE_CODE" ]; then
  SERVER=${SERVER%/}
  command -v curl >/dev/null 2>&1 || { echo "curl is required; install curl and run this command again." >&2; exit 1; }
  install_device
  exit 0
fi
if [ -z "$TOKEN" ] && [ -n "$TOKEN_ENV" ]; then
  eval "TOKEN=\${$TOKEN_ENV:-}"
fi
[ -n "$TOKEN" ] || usage
SERVER=${SERVER%/}

command -v curl >/dev/null 2>&1 || {
  echo "curl is required; install curl and run this command again." >&2
  exit 1
}

case "$(uname -s)" in
  Darwin) OS=darwin ;;
  Linux) OS=linux ;;
  *) echo "Unsupported operating system: $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

WEAVE_DIR="$HOME/.weave"
BIN_DIR="$WEAVE_DIR/bin"
BIN="$BIN_DIR/weave-runtime"
ENV_FILE="$WEAVE_DIR/runtime.env"
LOG_FILE="$WEAVE_DIR/runtime.log"
mkdir -p "$BIN_DIR"
TMP_BIN=$(mktemp "$BIN_DIR/.weave-runtime.XXXXXX")
cleanup_download() {
  rm -f "$TMP_BIN"
}
trap cleanup_download EXIT HUP INT TERM
download "$SERVER/v1/downloads/runtime/$OS/$ARCH" "$TMP_BIN"
chmod 755 "$TMP_BIN"
mv -f "$TMP_BIN" "$BIN"
trap - EXIT HUP INT TERM
{
  printf 'WEAVE_SERVER=%s\n' "$SERVER"
  printf 'WEAVE_RUNTIME_TOKEN=%s\n' "$TOKEN"
} > "$ENV_FILE"
chmod 600 "$ENV_FILE"

# Detect engine CLIs and export absolute paths: background services (launchd
# on macOS, systemd --user on Linux) run with a minimal PATH that excludes
# /opt/homebrew/bin, ~/.local/bin etc., so PATH-only detection would report
# zero engines. `export` is required — the launchd plist sources this file and
# plain assignments would not reach the runtime process; systemd
# EnvironmentFile accepts the optional `export` prefix.
ENGINE_SEARCH_PATH="$PATH:/opt/homebrew/bin:/usr/local/bin:$HOME/.local/bin:$HOME/bin"
for ENGINE in opencode codex claude; do
  UPPER=$(printf '%s' "$ENGINE" | tr '[:lower:]' '[:upper:]')
  ENGINE_PATH=""
  if command -v "$ENGINE" >/dev/null 2>&1; then
    ENGINE_PATH=$(command -v "$ENGINE")
  else
    OLD_IFS=$IFS
    IFS=:
    for DIR in $ENGINE_SEARCH_PATH; do
      if [ -n "$DIR" ] && [ -x "$DIR/$ENGINE" ]; then
        ENGINE_PATH="$DIR/$ENGINE"
        break
      fi
    done
    IFS=$OLD_IFS
  fi
  if [ -n "$ENGINE_PATH" ]; then
    printf 'export WEAVE_ENGINE_%s_PATH=%s\n' "$UPPER" "$ENGINE_PATH" >> "$ENV_FILE"
    echo "Detected engine: $ENGINE -> $ENGINE_PATH"
  else
    echo "Note: engine '$ENGINE' not found; install it later and add 'export WEAVE_ENGINE_${UPPER}_PATH=<path>' to $ENV_FILE, then restart the service."
  fi
done

METHOD=nohup
if [ "$OS" = darwin ]; then
  PLIST_DIR="$HOME/Library/LaunchAgents"
  PLIST="$PLIST_DIR/com.weave.runtime.plist"
  mkdir -p "$PLIST_DIR"
  cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>com.weave.runtime</string>
<key>ProgramArguments</key><array><string>/bin/sh</string><string>-c</string><string>. &quot;$ENV_FILE&quot; &amp;&amp; exec &quot;$BIN&quot; runtime --server &quot;\$WEAVE_SERVER&quot; --runtime-token &quot;\$WEAVE_RUNTIME_TOKEN&quot;</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
<key>StandardOutPath</key><string>$LOG_FILE</string>
<key>StandardErrorPath</key><string>$LOG_FILE</string>
</dict></plist>
EOF
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "Dry run: launchctl unload/load $PLIST"
  else
    launchctl unload "$PLIST" 2>/dev/null || true
    launchctl load "$PLIST"
  fi
  METHOD=launchd
elif command -v systemctl >/dev/null 2>&1; then
  SERVICE_DIR="$HOME/.config/systemd/user"
  SERVICE="$SERVICE_DIR/weave-runtime.service"
  mkdir -p "$SERVICE_DIR"
  cat > "$SERVICE" <<'EOF'
[Unit]
Description=Weave Runtime
[Service]
EnvironmentFile=%h/.weave/runtime.env
ExecStart=%h/.weave/bin/weave runtime --server ${WEAVE_SERVER} --runtime-token ${WEAVE_RUNTIME_TOKEN}
Restart=always
[Install]
WantedBy=default.target
EOF
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "Dry run: systemctl --user daemon-reload; systemctl --user enable --now weave-runtime.service"
    METHOD=systemd
  elif systemctl --user daemon-reload && systemctl --user enable --now weave-runtime.service; then
    METHOD=systemd
  fi
fi

if [ "$METHOD" = nohup ]; then
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "Dry run: nohup fallback would start the runtime"
  else
    nohup "$BIN" runtime --server "$SERVER" --runtime-token "$TOKEN" >> "$LOG_FILE" 2>&1 &
    echo "Using nohup as the background-process fallback; run this installer again after a machine restart."
  fi
fi

echo "Installed Weave runtime at $BIN"
echo "Background service: $METHOD"
if [ "$METHOD" = systemd ]; then
  echo "Status: systemctl --user status weave-runtime.service"
  echo "Logs: journalctl --user -u weave-runtime.service"
else
  echo "Logs: $LOG_FILE"
fi
echo "回到 Workbench 的运行节点设置，连接建立后即可查看节点状态"
