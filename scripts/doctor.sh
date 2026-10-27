#!/usr/bin/env bash
set -uo pipefail

install_root="${CLAWGOD_KIROCC_INSTALL_ROOT:-$HOME/.local/share/clawgod-kirocc}"
state_root="${CLAWGOD_KIROCC_STATE_ROOT:-$HOME/.clawgod-kirocc}"
user_bin_dir="${CLAWGOD_KIROCC_BIN_DIR:-$HOME/.local/bin}"
gateway_port="${KIROCC_PORT:-3456}"
gateway_url="${KIROCC_URL:-http://127.0.0.1:${gateway_port}}"
if [[ -n "${KIROCC_URL:-}" ]]; then
  gateway_url_label='<custom KIROCC_URL; value redacted>'
else
  gateway_url_label="$gateway_url"
fi
strict=false
runtime_kind=""
if [[ -f "$install_root/install-mode" ]]; then
  runtime_kind="$(tr -d '[:space:]' < "$install_root/install-mode")"
elif [[ -x "$install_root/clawgod/bin/clawgod" ]]; then
  runtime_kind="clawgod"
else
  runtime_kind="official"
fi
if [[ "$runtime_kind" != "official" && "$runtime_kind" != "clawgod" ]]; then
  runtime_kind="unknown"
fi

usage() {
  cat <<'USAGE'
Usage: ./scripts/doctor.sh [--strict]

Read-only checks for the ClaudeCode Kiro installation. It never prints tokens,
modifies configuration, starts Claude Code, or starts/stops the gateway.

  --strict  Return non-zero when warnings are present as well as failures.
USAGE
}

while (($# > 0)); do
  case "$1" in
    --strict) strict=true ;;
    -h|--help) usage; exit 0 ;;
    *) printf 'unknown option: %s\n' "$1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

pass_count=0
warn_count=0
fail_count=0

pass() {
  pass_count=$((pass_count + 1))
  printf '[PASS] %s\n' "$1"
}

warn() {
  warn_count=$((warn_count + 1))
  printf '[WARN] %s\n' "$1"
}

fail() {
  fail_count=$((fail_count + 1))
  printf '[FAIL] %s\n' "$1"
}

check_command() {
  if command -v "$1" >/dev/null 2>&1; then
    pass "command available: $1"
  else
    fail "missing command: $1"
  fi
}

check_executable() {
  if [[ -x "$1" ]]; then
    pass "executable: $1"
  else
    fail "missing executable: $1"
  fi
}

check_file() {
  if [[ -f "$1" ]]; then
    pass "file: $1"
  else
    fail "missing file: $1"
  fi
}

printf 'ClaudeCode Kiro doctor (read-only)\n'
printf '  install root: %s\n' "$install_root"
printf '  state root:   %s\n' "$state_root"
printf '  runtime:      %s\n' "$runtime_kind"
printf '  gateway URL:  %s\n\n' "$gateway_url_label"

required_commands=(go curl node python3)
if [[ "$runtime_kind" == "clawgod" ]]; then
  required_commands+=(bun rg)
fi
for required_command in "${required_commands[@]}"; do
  check_command "$required_command"
done
if [[ "$runtime_kind" == "clawgod" ]]; then
  if command -v shasum >/dev/null 2>&1 || command -v sha256sum >/dev/null 2>&1; then
    pass 'SHA-256 command available'
  else
    fail 'missing SHA-256 command: install shasum or sha256sum'
  fi
fi

official_claude="$(command -v claude 2>/dev/null || true)"
isolated_launcher="$(command -v claude-kiro 2>/dev/null || true)"
shared_entry_point="$(command -v kiroclaude 2>/dev/null || true)"
if [[ -n "$official_claude" ]]; then
  pass "official Claude command: $official_claude"
else
  fail 'official Claude command not found'
fi
if [[ -n "$isolated_launcher" ]]; then
  pass "isolated launcher: $isolated_launcher"
else
  fail "claude-kiro not found on PATH (expected under $user_bin_dir)"
fi
if [[ -n "$shared_entry_point" ]]; then
  pass "shared entry point: $shared_entry_point"
  # kiroclaude is installed as a link to claude-kiro, so both must launch the
  # same isolated launcher rather than two independently installed copies.
  if [[ -n "$isolated_launcher" ]]; then
    if [[ "$shared_entry_point" -ef "$isolated_launcher" ]]; then
      pass 'kiroclaude and claude-kiro launch the same isolated launcher'
    else
      fail 'kiroclaude and claude-kiro are separate files; rerun the installer'
    fi
  fi
else
  fail "kiroclaude not found on PATH (expected under $user_bin_dir)"
fi
if [[ -n "$official_claude" && -n "$isolated_launcher" ]]; then
  if [[ "$official_claude" == "$isolated_launcher" ]]; then
    fail 'official claude and claude-kiro resolve to the same path'
  else
    pass 'official claude and claude-kiro paths are separate'
  fi
  case "$official_claude" in
    "$install_root"/*|"$state_root"/*) fail 'official claude resolves inside the isolated roots' ;;
    *) pass 'official claude resolves outside the isolated roots' ;;
  esac
fi

check_executable "$install_root/bin/kirocc-native-websearch"
if [[ "$runtime_kind" == "clawgod" ]]; then
  check_executable "$install_root/clawgod/bin/clawgod"
  check_executable "$install_root/clawgod/bin/claude.orig"
  check_file "$state_root/cli.cjs"
elif [[ "$runtime_kind" == "official" ]]; then
  if [[ -n "$official_claude" ]]; then
    pass 'official Claude runtime selected'
  fi
else
  fail 'install mode is neither official nor clawgod'
fi
check_file "$state_root/claude-config/settings.json"
check_file "$state_root/claude-config/CLAUDE.md"

if [[ -f "$state_root/claude-config/settings.json" ]]; then
  if node -e 'JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"))' \
      "$state_root/claude-config/settings.json" >/dev/null 2>&1; then
    pass 'isolated settings.json is valid JSON'
  else
    fail 'isolated settings.json is invalid JSON'
  fi
fi

if [[ -n "${KIRO_API_KEY:-}" ]]; then
  pass 'Kiro API-key mode selected (value redacted)'
  if [[ -n "${KIRO_API_REGION:-}" ]]; then
    pass 'Kiro API region is set'
  else
    warn 'KIRO_API_REGION is unset; gateway will default to us-east-1'
  fi
else
  case "$(uname -s)" in
    Darwin) default_db="$HOME/Library/Application Support/kiro-cli/data.sqlite3" ;;
    Linux) default_db="$HOME/.local/share/kiro-cli/data.sqlite3" ;;
    *) default_db="" ;;
  esac
  kiro_db="${KIROCC_DB_PATH:-$default_db}"
  if [[ -n "$kiro_db" && -f "$kiro_db" ]]; then
    pass "Kiro CLI database found: $kiro_db"
    if command -v kiro-cli >/dev/null 2>&1; then
      pass 'Kiro CLI command is available for login maintenance'
      if kiro-cli whoami </dev/null >/dev/null 2>&1; then
        pass 'kiro-cli whoami confirms a logged-in profile (output redacted)'
      else
        fail 'kiro-cli whoami failed; run kiro-cli login, then kiro-cli whoami'
      fi
    else
      warn 'Kiro CLI command is missing; the gateway can use the existing database, but login repair requires Kiro CLI'
    fi
  elif [[ -n "$kiro_db" ]]; then
    fail "Kiro CLI database not found: $kiro_db"
    if command -v kiro-cli >/dev/null 2>&1; then
      warn 'run kiro-cli login, then kiro-cli whoami to create a usable login database'
    else
      warn 'install Kiro CLI and log in, or set KIRO_API_KEY and KIRO_API_REGION'
    fi
  else
    warn 'no default Kiro CLI database path for this OS; use KIROCC_DB_PATH or KIRO_API_KEY'
  fi
fi

case "$gateway_port" in
  ''|*[!0-9]*) fail 'KIROCC_PORT must be numeric' ;;
  *)
    gateway_port_number=$((10#$gateway_port))
    if ((gateway_port_number < 1 || gateway_port_number > 65535)); then
      fail 'KIROCC_PORT must be between 1 and 65535'
    else
      pass "gateway port is valid: $gateway_port"
    fi
    ;;
esac

if curl -fsS --noproxy '*' --max-time 1 "${gateway_url%/}/health" >/dev/null 2>&1; then
  pass 'gateway health endpoint is reachable'
  # /health answers unauthenticated and proves only that something is listening.
  # The serving contract lives behind the local bearer, so read it from the
  # status endpoint, which carries no credential, PID, path, prompt, or model
  # response and can never change readiness. Reading it spends no inference.
  status_document="$(curl -fsS --noproxy '*' --max-time 2 \
    -H "Authorization: Bearer ${KIROCC_API_KEY:-kiro-local}" \
    "${gateway_url%/}/v1/kirocc/status" 2>/dev/null)" || status_document=""
  if [[ -z "$status_document" ]]; then
    warn 'authenticated gateway status is unavailable; the listener predates the status endpoint or does not accept the local bearer'
  else
    status_summary="$(printf '%s' "$status_document" | node -e '
let raw = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => { raw += chunk; });
process.stdin.on("end", () => {
  let document;
  try { document = JSON.parse(raw); } catch { process.exit(3); }
  if (document.service !== "kirocc" || document.protocol_version !== 2) process.exit(4);
  const fields = ["auth_type", "region", "inference_host", "model_mapping_mode",
                  "upstream_probe_state", "upstream_ready"];
  process.stdout.write(fields.map((field) => field + "=" + document[field]).join(" "));
  process.exit(document.upstream_ready === true ? 0 : 5);
});
' 2>/dev/null)"
    case "$?" in
      0) pass "gateway serving contract: $status_summary" ;;
      5) warn "gateway is serving but upstream readiness is unproven: $status_summary" ;;
      *) fail 'gateway status is not the expected kirocc protocol 2 document' ;;
    esac
  fi
else
  warn 'gateway is not currently reachable; run kiroclaude --gateway-start'
fi

record_path="$HOME/.cache/claudecode-kiro/gateway-${gateway_port}.json"
if [[ -f "$record_path" ]]; then
  # Diagnostics only. A record is never readiness authority and never authorizes
  # a signal, so anything it says here is informational, never pass or fail.
  record_summary="$(node -e '
const fs = require("fs");
let document;
try { document = JSON.parse(fs.readFileSync(process.argv[1], "utf8")); } catch { process.exit(3); }
if (typeof document.gateway_sha256 === "string") {
  document.gateway_sha256 = document.gateway_sha256.slice(0, 12) + "...";
}
// A record written by an older launcher carries fewer fields. Report what is
// there rather than inventing an "undefined" for what is not.
const present = ["pid", "port", "region", "spawned_at", "gateway_sha256"]
  .filter((field) => document[field] !== undefined && document[field] !== null)
  .map((field) => field + "=" + document[field]);
process.stdout.write(present.length > 0 ? present.join(" ") : "record carries no recognized field");
' "$record_path" 2>/dev/null)" || record_summary=""
  if [[ -n "$record_summary" ]]; then
    printf '\nLast recorded gateway spawn (informational): %s\n' "$record_summary"
  else
    printf '\nLast recorded gateway spawn (informational): unreadable record at %s\n' "$record_path"
  fi
fi

printf '\nSummary: %d pass, %d warning, %d failure\n' \
  "$pass_count" "$warn_count" "$fail_count"

if ((fail_count > 0)); then
  exit 1
fi
if [[ "$strict" == true && "$warn_count" -gt 0 ]]; then
  exit 2
fi
