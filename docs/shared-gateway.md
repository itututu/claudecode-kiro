# Shared gateway on macOS and Linux

`./scripts/install.sh` installs `claude-kiro` and the `kiroclaude` alias. Both run
the installed official Claude Code runtime by default, with the isolated Claude
settings directory. The runtime installation is not modified. Python 3 is a
launcher prerequisite; ClawGod remains an explicit installer opt-in.

```sh
kiroclaude --gateway-start   # start once, or verify and reuse the existing gateway
kiroclaude --gateway-status  # check local readiness/authentication without starting it
kiroclaude                  # open an official Claude Code session
claude-kiro -p 'Reply exactly OK'  # verify actual upstream inference
```

Every pane shares `http://127.0.0.1:3456` by default. Exiting a pane leaves the
gateway running. A kernel file lock coordinates simultaneous launches and is
released automatically after a launcher crash. The gateway is detached from the
launcher's terminal, with its stdin closed and output written to a private log.
It is not a login or boot service; the first launch after reboot starts it again.

The launcher requires `/health` and the authenticated Kiro `/v1/models` response,
and verifies that both missing and incorrect local credentials receive HTTP 401.
Those are local checks only, so one launcher additionally spends a single real
upstream request to prove inference works, marked `X-Kirocc-Readiness-Probe: 1`.
Each gateway process answers exactly one such probe: the first launcher to claim
it runs it, later launchers reuse the recorded outcome, and `--gateway-status`
never spends one. Readiness survives only in the gateway process, so a restarted
gateway is probed again. An occupied, unauthenticated, wrong-key, or incompatible
service is refused without killing it.
A managed process using a replaced gateway binary or another region must be
restarted deliberately after existing sessions have finished. Inspect the owned
process before stopping it; the launcher never kills a process based on its port.

`GET /v1/kirocc/status`, authenticated with the local token, reports the serving
contract: auth type, region, inference host, model-mapping mode, and the probe
state above. It carries no credential, PID, path, prompt, or model response, and
reading it can never change readiness. `./scripts/doctor.sh` reports it, and a
mapping mode of `custom` makes the launcher refuse to reuse a managed gateway.

Gateway logs are in `~/.clawgod-kirocc/gateway/gateway-3456.log`. Process records and
lock files are in `~/.cache/claudecode-kiro/`. They contain no credentials, and a
record is diagnostic only: it never establishes readiness and never authorizes a
signal, so a PID in it may belong to a process that is already gone. Do not
unlink a lock file while launchers are running. A stale file is harmless because
the lock belongs to an open file descriptor, not to the file's presence.
`./scripts/uninstall.sh` removes both, and never stops a running gateway.

## Environment

- `KIRO_API_KEY`: actual upstream Kiro credential, supplied through the shell
  environment or a private credential store. It is passed only to a new gateway,
  never on a command line or to the Claude runtime. An API key starts with `ksk_`.
- `KIROCC_API_KEY`: local gateway token; defaults to `kiro-local`. It is explicitly
  supplied to both the gateway and Claude, even if not exported by the shell.
- `KIROCC_PORT`: port shared by these launchers; defaults to `3456`. The gateway
  applies environment overrides after command-line flags, so an exported value
  overrides `-port` in direct gateway invocations.
- `KIRO_API_REGION`: region for a new gateway; defaults to `us-east-1`. Changing
  the shell variable does not change credentials or region in a running gateway.
- `CLAUDE_KIRO_MODEL`, `CLAUDE_KIRO_SONNET_MODEL`, `CLAUDE_KIRO_OPUS_MODEL`, and
  `CLAUDE_KIRO_HAIKU_MODEL`: deliberate model choices. Defaults are Sonnet 4.6,
  Sonnet 4.6, Opus 4.6, and Haiku 4.5 respectively, using Claude's canonical IDs
  `claude-sonnet-4-6`, `claude-opus-4-6`, and `claude-haiku-4-5`. The gateway maps
  those IDs to Kiro models. The Haiku choice also controls
  `ANTHROPIC_SMALL_FAST_MODEL`; actual account/model access requires inference.

Inherited `ANTHROPIC_*`, `CLAUDE_CODE_USE_*`, and Claude OAuth token variables are
removed before assigning the local provider configuration. Proxy variables are
removed from Claude by default so localhost requests stay local; the gateway can
still inherit proxy settings for upstream traffic. `KIROCC_URL`, if supplied,
must match the selected `http://127.0.0.1:KIROCC_PORT` address.

Ensure `~/.local/bin` is on the PATH in new terminals and remove obsolete shell
functions named `kiroclaude` that shadow the installed launcher. Store the real
Kiro key as the password of a private credential-store item with a nonsecret
service name; never use the key itself as that item's service name.
