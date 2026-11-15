#!/bin/sh
# Installs the scan worker as a launchd agent that starts at login and restarts if
# it exits. The worker keeps the Mac from idle-sleeping (caffeinate) while it runs,
# and hands its current order back to the gateway when macOS shuts down.
#   sh install.sh [/path/to/python]     (default: the babeldoc conda env)
set -eu
here=$(cd "$(dirname "$0")" && pwd)
python=${1:-$HOME/miniconda3/envs/babeldoc/bin/python}
env_file=$HOME/.config/pdfmath-scan-worker.env
plist=$HOME/Library/LaunchAgents/com.pdfmath.scan-worker.plist
log=$HOME/Library/Logs/pdfmath-scan-worker.log

"$python" -c 'import pymupdf, openai, httpx' || { echo "missing packages in $python"; exit 1; }
if [ ! -f "$env_file" ]; then
  mkdir -p "$(dirname "$env_file")"
  cp "$here/scan-worker.env.example" "$env_file"
  chmod 600 "$env_file"
  echo "Created $env_file - fill in SCAN_WORKER_TOKEN and OPENAI_API_KEY, then run this again."
  exit 0
fi
mkdir -p "$HOME/Library/LaunchAgents" "$HOME/Library/Logs"
cat > "$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.pdfmath.scan-worker</string>
  <key>ProgramArguments</key>
  <array>
    <string>$python</string><string>$here/scan_worker.py</string><string>run</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict><key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>30</integer>
  <key>ExitTimeOut</key><integer>20</integer>
  <key>StandardOutPath</key><string>$log</string>
  <key>StandardErrorPath</key><string>$log</string>
</dict>
</plist>
PLIST
launchctl bootout "gui/$(id -u)/com.pdfmath.scan-worker" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$plist"
echo "Scan worker running. Log: $log"
echo "Stop: launchctl bootout gui/$(id -u)/com.pdfmath.scan-worker"
