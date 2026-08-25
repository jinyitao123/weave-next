#!/bin/sh
set -eu

usage() {
  echo "Usage: install.sh --server <url> (--token <rtk_...> | --token-env <env-name>) [--dry-run]" >&2
  exit 1
}

SERVER=
TOKEN=
TOKEN_ENV=
DRY_RUN=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --server) [ "$#" -ge 2 ] || usage; SERVER=$2; shift 2 ;;
    --token) [ "$#" -ge 2 ] || usage; TOKEN=$2; shift 2 ;;
    --token-env) [ "$#" -ge 2 ] || usage; TOKEN_ENV=$2; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    *) usage ;;
  esac
done
[ -n "$SERVER" ] || usage
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
curl -fsSL --retry 3 --retry-all-errors "$SERVER/v1/downloads/runtime/$OS/$ARCH" -o "$TMP_BIN"
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
echo "回到 Weave 控制台的 设置 → 多运行时配置 查看该运行时将在数秒内转为在线"
