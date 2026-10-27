#!/usr/bin/env bash
set -euo pipefail

install_root="${CLAWGOD_KIROCC_INSTALL_ROOT:-$HOME/.local/share/clawgod-kirocc}"
state_root="${CLAWGOD_KIROCC_STATE_ROOT:-$HOME/.clawgod-kirocc}"
user_bin_dir="${CLAWGOD_KIROCC_BIN_DIR:-$HOME/.local/bin}"
purge_state=false

if [[ "${1:-}" == "--purge-state" ]]; then
  purge_state=true
elif (($# > 0)); then
  printf 'Usage: ./scripts/uninstall.sh [--purge-state]\n' >&2
  exit 2
fi

validate_removal_root() {
  local candidate="$1"
  local expected_leaf="$2"
  if [[ -z "$candidate" || "$candidate" == "/" || "$candidate" == "$HOME" || "${candidate##*/}" != "$expected_leaf" ]]; then
    printf 'refusing unsafe removal target: %s\n' "$candidate" >&2
    exit 2
  fi
}

validate_removal_root "$install_root" "clawgod-kirocc"
rm -f "$user_bin_dir/claude-kiro" "$user_bin_dir/kiroclaude"
rm -rf "$install_root"

# Startup locks and process records describe a gateway binary that no longer
# exists after the removal above, so they go with it. They are diagnostics, never
# authority, so nothing here inspects or signals a running gateway: a gateway
# started before this uninstall keeps serving until it exits on its own.
lock_dir="$HOME/.cache/claudecode-kiro"
if [[ -d "$lock_dir" ]]; then
  validate_removal_root "$lock_dir" "claudecode-kiro"
  rm -rf "$lock_dir"
fi

if [[ "$purge_state" == true ]]; then
  validate_removal_root "$state_root" ".clawgod-kirocc"
  rm -rf "$state_root"
else
  printf 'Preserved state: %s\n' "$state_root"
fi

printf 'Removed ClaudeCode Kiro runtime. Official Claude Code was not touched.\n'
