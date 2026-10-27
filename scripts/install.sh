#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
install_root="${CLAWGOD_KIROCC_INSTALL_ROOT:-$HOME/.local/share/clawgod-kirocc}"
state_root="${CLAWGOD_KIROCC_STATE_ROOT:-$HOME/.clawgod-kirocc}"
user_bin_dir="${CLAWGOD_KIROCC_BIN_DIR:-$HOME/.local/bin}"
gateway_port="${KIROCC_PORT:-3456}"
clawgod_release="${CLAWGOD_RELEASE:-v1.7.5}"
clawgod_installer_sha256="${CLAWGOD_INSTALLER_SHA256:-4a943439ae8cb858e69279d19f0d3a979968fc0a9e4c42e1d1018ae76657ce82}"
refresh_clawgod=false
gateway_only=false
with_clawgod=false

usage() {
  cat <<'USAGE'
Usage: ./scripts/install.sh [--with-clawgod] [--refresh-clawgod] [--gateway-only]

  --with-clawgod      Install the isolated ClawGod runtime (opt-in).
  --refresh-clawgod  Rebuild the isolated ClawGod runtime even when present.
  --gateway-only     Use an existing explicit CLAWGOD_BIN (legacy advanced mode).

Environment overrides:
  CLAWGOD_BIN                    Existing explicit ClawGod launcher.
  CLAUDE_KIRO_RUNTIME_BIN         Explicit official Claude executable.
  CLAWGOD_RELEASE                ClawGod release tag (default: v1.7.5).
  CLAWGOD_INSTALLER_SHA256       Required checksum when changing the release.
  CLAWGOD_KIROCC_INSTALL_ROOT    Runtime installation root.
  CLAWGOD_KIROCC_STATE_ROOT      Isolated ClawGod state/config root.
  CLAWGOD_KIROCC_BIN_DIR         Directory for the claude-kiro launcher.
  KIROCC_PORT                    Gateway port (default: 3456).
  KIROCC_DB_PATH                 Existing Kiro CLI login database.
  KIRO_API_KEY                   Kiro upstream API key; makes Kiro CLI optional.
  KIRO_API_REGION                Kiro API-key region (default: us-east-1).
USAGE
}

while (($# > 0)); do
  case "$1" in
    --with-clawgod) with_clawgod=true ;;
    --refresh-clawgod) refresh_clawgod=true; with_clawgod=true ;;
    --gateway-only) gateway_only=true; with_clawgod=true ;;
    -h|--help) usage; exit 0 ;;
    *) printf 'unknown option: %s\n' "$1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

if [[ "$gateway_only" == true && -z "${CLAWGOD_BIN:-}" ]]; then
  printf 'CLAWGOD_BIN is required with --gateway-only\n' >&2
  exit 2
fi

case "$gateway_port" in
  ''|*[!0-9]*) printf 'KIROCC_PORT must be numeric\n' >&2; exit 2 ;;
esac
gateway_port_number=$((10#$gateway_port))
if ((gateway_port_number < 1 || gateway_port_number > 65535)); then
  printf 'KIROCC_PORT must be between 1 and 65535\n' >&2
  exit 2
fi

required_commands=(go curl node python3)
if [[ "$with_clawgod" == true ]]; then
  required_commands+=(bun rg)
fi
for required_command in "${required_commands[@]}"; do
  if ! command -v "$required_command" >/dev/null 2>&1; then
    printf 'missing prerequisite: %s\n' "$required_command" >&2
    exit 127
  fi
done

official_claude_command="${CLAUDE_KIRO_RUNTIME_BIN:-$(command -v claude 2>/dev/null || true)}"
if [[ -z "$official_claude_command" || ! -x "$official_claude_command" || -d "$official_claude_command" ]]; then
  printf 'official Claude Code command not found\n' >&2
  exit 127
fi
# A leftover .orig sibling may belong to an obsolete npm or wrapper install.
# Resolve the selected executable itself; wrappers require an explicit override.
official_claude_candidate="$official_claude_command"
official_claude_real="$(node -e 'console.log(require("fs").realpathSync(process.argv[1]))' "$official_claude_candidate")"

case "$(uname -s)" in
  Darwin) default_kiro_db="$HOME/Library/Application Support/kiro-cli/data.sqlite3" ;;
  Linux) default_kiro_db="$HOME/.local/share/kiro-cli/data.sqlite3" ;;
  *) default_kiro_db="" ;;
esac
kiro_db_path="${KIROCC_DB_PATH:-$default_kiro_db}"
if [[ -n "${KIRO_API_KEY:-}" ]]; then
  printf 'Kiro authentication: API-key mode (Kiro CLI is not required).\n'
elif [[ -n "$kiro_db_path" && -f "$kiro_db_path" ]]; then
  if command -v kiro-cli >/dev/null 2>&1; then
    if kiro-cli whoami </dev/null >/dev/null 2>&1; then
      printf 'Kiro authentication: existing login confirmed (whoami output redacted).\n'
    else
      cat >&2 <<EOF
Kiro CLI database exists, but kiro-cli whoami did not confirm a logged-in profile.
Run:

  kiro-cli login
  kiro-cli whoami

Then rerun this installer. Request traffic will still be sent directly by the
kirocc gateway; this check only validates the credential it will read.
EOF
      exit 78
    fi
  else
    printf 'Kiro authentication: existing login database found (Kiro CLI command unavailable; live status not checked).\n'
  fi
else
  if command -v kiro-cli >/dev/null 2>&1; then
    kiro_cli_state='Kiro CLI is installed, but its login database was not found.'
  else
    kiro_cli_state='Kiro CLI is not installed and no existing login database was found.'
  fi
  cat >&2 <<EOF
No usable Kiro credential source found.
$kiro_cli_state

Kiro CLI is used only to create the local login credential; request traffic is
sent directly by the kirocc gateway. Install/login with:

  curl -fsSL https://cli.kiro.dev/install | bash
  kiro-cli login
  kiro-cli whoami

Or set KIRO_API_KEY (and optionally KIRO_API_REGION) before running this
installer and whenever claude-kiro is launched.
Expected database: ${kiro_db_path:-<no default for this OS>}
EOF
  exit 78
fi

sha256_file() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  elif command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    printf 'missing prerequisite: shasum or sha256sum\n' >&2
    return 127
  fi
}

official_claude_sha256_before="$(node -e '
const fs = require("fs");
const crypto = require("crypto");
const hash = crypto.createHash("sha256");
hash.update(fs.readFileSync(process.argv[1]));
console.log(hash.digest("hex"));
' "$official_claude_real")"

mkdir -p "$install_root/bin" "$state_root/claude-config" "$user_bin_dir"
if [[ "$with_clawgod" == true ]]; then
  mkdir -p "$install_root/clawgod/bin"
fi

temporary_root="$(mktemp -d "${TMPDIR:-/tmp}/clawgod-kirocc.XXXXXX")"
cleanup_install() {
  rm -rf "$temporary_root"
}
trap cleanup_install EXIT INT TERM HUP

printf 'Building patched kirocc...\n'
GOEXPERIMENT=jsonv2 go build -trimpath -o "$temporary_root/kirocc" "$project_root/cmd/kirocc"
install -m 0755 "$temporary_root/kirocc" "$install_root/bin/kirocc-native-websearch"

runtime_kind="official"
runtime_bin="$official_claude_real"
runtime_clawgod_bin="${CLAWGOD_BIN:-$install_root/clawgod/bin/clawgod}"
if [[ "$with_clawgod" == true && "$gateway_only" == false && ("$refresh_clawgod" == true || ! -x "$runtime_clawgod_bin") ]]; then
  if [[ "$clawgod_release" != "v1.7.5" && -z "${CLAWGOD_INSTALLER_SHA256:-}" ]]; then
    printf 'CLAWGOD_INSTALLER_SHA256 is required for ClawGod releases other than v1.7.5\n' >&2
    exit 2
  fi

  clawgod_installer="$temporary_root/clawgod-install.sh"
  clawgod_url="https://github.com/0Chencc/clawgod/releases/download/${clawgod_release}/install.sh"
  printf 'Downloading ClawGod %s installer...\n' "$clawgod_release"
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
    --output "$clawgod_installer" "$clawgod_url"

  actual_sha256="$(sha256_file "$clawgod_installer")"
  if [[ "$actual_sha256" != "$clawgod_installer_sha256" ]]; then
    printf 'ClawGod installer checksum mismatch\nexpected: %s\nactual:   %s\n' \
      "$clawgod_installer_sha256" "$actual_sha256" >&2
    exit 1
  fi

  # Add three opt-in path overrides to the downloaded GPL installer. The
  # modified installer runs only from the temporary directory and is never
  # redistributed. Its output remains under the isolated roots above.
  node - "$clawgod_installer" <<'NODE'
const fs = require('fs');
const path = process.argv[2];
let source = fs.readFileSync(path, 'utf8');
const replacements = [
  ['CLAWGOD_DIR="$HOME/.clawgod"', 'CLAWGOD_DIR="${CLAWGOD_DIR_OVERRIDE:-$HOME/.clawgod}"'],
  ['BIN_DIR="$HOME/.local/bin"', 'BIN_DIR="${CLAWGOD_BIN_DIR_OVERRIDE:-$HOME/.local/bin}"'],
  ['CLAUDE_BIN=$(command -v claude 2>/dev/null || true)', 'CLAUDE_BIN="${CLAWGOD_CLAUDE_BIN_OVERRIDE:-$(command -v claude 2>/dev/null || true)}"'],
  ['CLAUDE_SETTINGS="$HOME/.claude/settings.json"', 'CLAUDE_SETTINGS="${CLAWGOD_CLAUDE_SETTINGS_OVERRIDE:-$HOME/.claude/settings.json}"'],
  ['CLAUDE_SETTINGS_DIR="$HOME/.claude"', 'CLAUDE_SETTINGS_DIR="${CLAWGOD_CLAUDE_SETTINGS_DIR_OVERRIDE:-$HOME/.claude}"'],
  ["const clawgodDir = join(homedir(), '.clawgod');", "const clawgodDir = process.env.CLAWGOD_RUNTIME_STATE_ROOT || join(homedir(), '.clawgod');"],
  ["const nativeClaudeJson = join(homedir(), '.claude.json');", "const nativeClaudeJson = join(process.env.CLAUDE_CONFIG_DIR || homedir(), '.claude.json');"],
];
for (const [before, after] of replacements) {
  if (!source.includes(before)) throw new Error(`unsupported ClawGod installer: missing ${before}`);
  source = source.split(before).join(after);
}
fs.writeFileSync(path, source);
NODE

  CLAWGOD_DIR_OVERRIDE="$state_root" \
  CLAWGOD_BIN_DIR_OVERRIDE="$install_root/clawgod/bin" \
  CLAWGOD_CLAUDE_BIN_OVERRIDE="$install_root/clawgod/bin/claude" \
  CLAWGOD_CLAUDE_SETTINGS_OVERRIDE="$state_root/claude-config/settings.json" \
  CLAWGOD_CLAUDE_SETTINGS_DIR_OVERRIDE="$state_root/claude-config" \
  CLAWGOD_RUNTIME_STATE_ROOT="$state_root" \
  CLAUDE_CONFIG_DIR="$state_root/claude-config" \
    bash "$clawgod_installer" --lean-off

  # ClawGod uses claude.orig for shell integration and child dispatch. Create
  # that reference inside the isolated prefix without altering the official
  # launcher or its adjacent files.
  ln -sfn "$official_claude_real" "$install_root/clawgod/bin/claude.orig"

  # ClawGod's generated JS wrapper normally derives ~/.clawgod from HOME.
  # Point this local copy at the isolated state root and respect our separate
  # Claude configuration directory.
  node - "$state_root/cli.cjs" "$state_root" <<'NODE'
const fs = require('fs');
const path = process.argv[2];
const stateRoot = process.argv[3];
let source = fs.readFileSync(path, 'utf8');
const dirNeedles = [
  "const clawgodDir = process.env.CLAWGOD_RUNTIME_STATE_ROOT || join(homedir(), '.clawgod');",
  "const clawgodDir = join(homedir(), '.clawgod');",
];
const dirNeedle = dirNeedles.find(needle => source.includes(needle));
if (!dirNeedle) throw new Error('unsupported ClawGod wrapper: state root marker not found');
source = source.replace(dirNeedle, `const clawgodDir = ${JSON.stringify(stateRoot)};`);
source = source.replace(
  "const nativeClaudeJson = join(homedir(), '.claude.json');",
  "const nativeClaudeJson = join(process.env.CLAUDE_CONFIG_DIR || homedir(), '.claude.json');",
);
fs.writeFileSync(path, source);
NODE

  # Disable in-place updates: rerun this installer so the same isolation and
  # checksum checks are applied to every refresh.
  for generated_launcher in "$install_root/clawgod/bin/clawgod" "$install_root/clawgod/bin/claude"; do
    node - "$generated_launcher" <<'NODE'
const fs = require('fs');
const path = process.argv[2];
let source = fs.readFileSync(path, 'utf8');
const marker = '# Route \'import\' subcommand to clawgod-import binary';
const guard = `if [ "\${1:-}" = "update" ]; then
  echo "clawgod-kirocc: run the project installer with --refresh-clawgod to update safely" >&2
  exit 2
fi
`;
if (!source.includes(marker)) throw new Error('unsupported ClawGod launcher: marker not found');
source = source.replace(marker, guard + marker);
fs.writeFileSync(path, source);
NODE
    chmod 0755 "$generated_launcher"
  done
  runtime_clawgod_bin="$install_root/clawgod/bin/clawgod"
fi

if [[ "$with_clawgod" == true && ! -x "$runtime_clawgod_bin" ]]; then
  printf 'ClawGod launcher not found: %s\nSet CLAWGOD_BIN or omit --gateway-only.\n' "$runtime_clawgod_bin" >&2
  exit 127
fi
if [[ "$with_clawgod" == true ]]; then
  runtime_kind="clawgod"
  runtime_bin="$runtime_clawgod_bin"
fi

if [[ ! -f "$state_root/claude-config/CLAUDE.md" ]]; then
  install -m 0644 "$project_root/config/CLAUDE.md" "$state_root/claude-config/CLAUDE.md"
fi
if [[ ! -f "$state_root/claude-config/settings.json" ]]; then
  install -m 0644 "$project_root/config/settings.json" "$state_root/claude-config/settings.json"
else
  # Installation is the only place an existing isolated profile is rewritten.
  # The launcher refuses to serve "model": "fable", so drop exactly that
  # selection and leave every other key, including any other model, alone.
  python3 - "$state_root/claude-config/settings.json" <<'PYSETTINGS'
import json, os, sys, tempfile
from pathlib import Path
path = Path(sys.argv[1])
try:
    document = json.loads(path.read_text())
except (ValueError, UnicodeError):
    print("warning: isolated settings are not valid JSON; leaving them untouched", file=sys.stderr)
    sys.exit(0)
if not isinstance(document, dict) or document.get("model") != "fable":
    sys.exit(0)
del document["model"]
handle, temporary = tempfile.mkstemp(dir=str(path.parent), prefix=".settings-")
with os.fdopen(handle, "w") as file:
    file.write(json.dumps(document, indent=2) + "\n")
os.chmod(temporary, 0o644)
os.replace(temporary, path)
print('migrated isolated settings: removed unservable "model": "fable"')
PYSETTINGS
fi

launcher_path="$user_bin_dir/claude-kiro"
launcher_impl="$install_root/bin/claude-kiro.py"
install -m 0755 "$project_root/scripts/claude-kiro.py" "$launcher_impl"
python3 - "$launcher_impl" "$install_root/bin/kirocc-native-websearch" "$runtime_bin" "$state_root" "$gateway_port" "$launcher_path" <<'PYLAUNCHER'
import json
from pathlib import Path
import shlex
import sys
implementation, gateway, runtime, state, port, launcher = sys.argv[1:]
configuration = {
    "gateway_bin": gateway, "runtime_bin": runtime,
    "config_dir": str(Path(state) / "claude-config"), "state_root": state,
    "gateway_port": int(port),
}
Path(implementation).with_suffix(".json").write_text(json.dumps(configuration, indent=2) + "\n")
Path(launcher).write_text("#!/usr/bin/env bash\nset -euo pipefail\nexec python3 " + shlex.quote(implementation) + ' "$@"\n')
PYLAUNCHER
chmod 0755 "$launcher_path"
ln -sfn "claude-kiro" "$user_bin_dir/kiroclaude"
printf '%s\n' "$runtime_kind" > "$install_root/install-mode"

official_claude_sha256_after="$(node -e '
const fs = require("fs");
const crypto = require("crypto");
const hash = crypto.createHash("sha256");
hash.update(fs.readFileSync(process.argv[1]));
console.log(hash.digest("hex"));
' "$official_claude_real")"
if [[ "$official_claude_sha256_after" != "$official_claude_sha256_before" ]]; then
  printf 'official Claude Code changed during installation; refusing to report success\n' >&2
  exit 1
fi

printf '\nInstalled successfully:\n'
printf '  launcher: %s\n' "$launcher_path"
printf '  gateway:  %s\n' "$install_root/bin/kirocc-native-websearch"
printf '  runtime:  %s (%s)\n' "$runtime_bin" "$runtime_kind"
printf '  config:    %s\n' "$state_root/claude-config"
printf '  commands:  claude-kiro, kiroclaude (shared persistent gateway)\n\n'
printf 'The official claude command was not modified by this installer.\n'
