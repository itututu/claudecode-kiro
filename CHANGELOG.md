# Changelog

All notable changes to this fork are documented here. The project follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and uses fork-specific
pre-release tags based on the upstream kirocc version.

## [Unreleased]

### Changed

- The public repository and current project branding are now `claudecode-kiro`
  / ClaudeCode Kiro. Existing release tags, artifact names, commands, and local
  installation paths remain unchanged for compatibility.
- Managed launchers now default Kiro runtime and MCP traffic to `us-east-1`;
  `KIRO_API_REGION=eu-central-1` remains an explicit override.

### Fixed

- `KIRO_API_REGION` now overrides runtime and MCP endpoint routing for Kiro CLI
  database credentials as well as API-key auth, while credential refresh keeps
  using the original login region. This prevents 502s when the login region has
  no Kiro inference host.
- Kiro-style always-1M aliases such as `claude-opus-4.8[1m]` and `[1M]` now
  resolve exactly without accidentally enabling thinking.
- `claude-opus-5-5` / `claude-opus-5-5[1m]` now map to Kiro's always-1M
  `claude-opus-5.5` SKU with the five-level effort enum, so `high`/`xhigh`/`max`
  are forwarded natively instead of dropped (no `KIROCC_MODEL_MAPPINGS` needed).
- `claude-sonnet-5-5` / `claude-sonnet-5-5[1m]` likewise map to Kiro's
  always-1M `claude-sonnet-5.5` SKU with the five-level effort enum.
- `claude-sonnet-4-6[1m]` and the `context-1m` header no longer route to the
  legacy `claude-sonnet-4.6-1m` SKU, which accepts no effort fields and silently
  dropped every requested effort. Sonnet 4.6 now uses its always-1M base SKU.
- `claude-sonnet-4.5[1m]` no longer routes to `claude-sonnet-4.5-1m`, which Kiro
  rejects as unavailable; Sonnet 4.5 is 200k only.
- Dated snapshot IDs such as Claude Code's `claude-haiku-4-5-20251001`, and
  dashed 4.5-family IDs (`claude-haiku-4-5`, `claude-sonnet-4-5`,
  `claude-opus-4-5`), now resolve to Kiro SKUs instead of passing through to an
  "model not available" error.
- Per-model effort enums re-verified against the kiro-cli 2.27.1 catalog.
- Claude Code's new default `claude-opus-5[1m]` model now maps explicitly to
  Kiro's `claude-opus-5` SKU with the supported five-level effort enum; its
  context suffix no longer falls through the legacy thinking path.
- Managed launchers now preserve HTTP(S) proxy variables in the gateway process
  but remove them from the Claude Code child when `KIROCC_URL` is loopback. This
  prevents a system proxy from intercepting `127.0.0.1` and producing a 502 with
  no corresponding gateway request. Set `CLAUDE_KIRO_PRESERVE_PROXY=1` to opt out.

## [v0.6.0-clawgod.2] - 2026-08-03

### Changed

- Documentation now makes the traffic boundary explicit: Kiro CLI is only a
  login-database bootstrap/maintenance tool, while the kirocc gateway sends
  chat and WebSearch requests directly to Kiro.

### Fixed

- The macOS/Linux and Windows installers now reject a missing Kiro credential
  source before building, while preserving Kiro API-key mode with no Kiro CLI
  dependency.
- Generated launchers now fail with actionable Kiro login instructions when a
  local gateway would start without a database or API key.
- Launchers authenticate an already-running gateway's `/v1/models` endpoint so
  a stale process with a different `KIROCC_API_KEY` is reported before Claude
  Code encounters a misleading local 401.
- Doctor scripts now check `kiro-cli whoami` without displaying account output
  and distinguish an optional missing CLI command from a missing login database.
- The Windows installer now reports the exact active `go.exe` path and version,
  and rejects Go versions older than the `go.mod` requirement before attempting
  a `GOEXPERIMENT=jsonv2` build.

## [v0.6.0-clawgod.1] - 2026-08-03

### Added

- An isolated `claude-kiro` launcher and configuration profile that use the
  official Claude Code runtime by default and leave `claude` untouched.
- Native Windows 11 x64 PowerShell install, launcher, doctor, uninstall, CI,
  and release support.
- A pinned, checksum-verified ClawGod v1.7.5 installation flow with generated
  runtime files kept outside Git.
- Kiro-native WebSearch translation for Anthropic server-tool requests,
  including non-streaming and SSE responses, credential refresh, and retry
  handling.
- A read-only `scripts/doctor.sh` command for installation, isolation,
  credential-source, port, and gateway-health diagnostics.
- English and Chinese setup, architecture, capability, security, and
  troubleshooting documentation.

### Changed

- GitHub now renders the complete Chinese documentation by default from
  `README.md`; the complete English documentation is available in
  `README_EN.md`, and the first fork release draft is Chinese-first.
- ClawGod is now an explicit optional component selected with
  `--with-clawgod` / `-WithClawGod`; it is no longer downloaded by default.
- The managed gateway uses port `3457` by default to avoid the upstream
  standalone default on `3456`.
- The installer accepts either `shasum` or `sha256sum` for macOS/Linux
  checksum verification.
- The release workflow accepts fork tags matching `v*-clawgod.*`; inherited
  upstream tags must not be republished.
- GoReleaser publishes Windows binaries as ZIP archives in addition to the
  macOS/Linux tarballs.

### Security

- Generated Claude Code/ClawGod files, credentials, provider files, sessions,
  and logs are excluded from the repository.
- In-place `claude-kiro update` is blocked to preserve the isolated path and
  verified installer boundary.

[Unreleased]: https://github.com/itututu/claudecode-kiro/compare/v0.6.0-clawgod.2...HEAD
[v0.6.0-clawgod.2]: https://github.com/itututu/claudecode-kiro/compare/v0.6.0-clawgod.1...v0.6.0-clawgod.2
[v0.6.0-clawgod.1]: https://github.com/itututu/claudecode-kiro/releases/tag/v0.6.0-clawgod.1
