#!/usr/bin/env python3
"""Launch the installed Claude runtime through one authenticated, shared gateway.

Only the gateway inherits KIRO_API_KEY. Credentials never appear in arguments,
launcher configuration, process records, or launcher diagnostics.
"""
import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path
import secrets
import socket
import stat
import subprocess
import sys
import time
import urllib.error
import urllib.request


# The local bearer is a fixed non-upstream value. It authenticates loopback
# callers to the gateway and is never a Kiro credential.
LOCAL_BEARER = "kiro-local"
STATUS_PROTOCOL_VERSION = 2
READINESS_HEADER = "X-Kirocc-Readiness-Probe"
SESSION_HEADER = "X-Claude-Code-Session-Id"
# Validated serving regions. The inference host is derived, never probed.
SERVING_REGIONS = ("us-east-1", "eu-central-1")
DEFAULT_SERVING_REGION = "us-east-1"
PROBE_MODEL = "claude-sonnet-4.6"
DEFAULT_SONNET_MODEL = "claude-sonnet-4-6"
# Environment families that must not cross into a child process: they would
# re-point routing, re-key authentication, or export telemetry we did not ask
# for. KIRO_API_KEY and KIROCC_DB_PATH are re-added explicitly, gateway only.
GATEWAY_STRIP_PREFIXES = ("ANTHROPIC_", "CLAUDE_CODE_", "CLAUDE_KIRO_", "KIROCC_", "OTEL_")
RUNTIME_STRIP_PREFIXES = ("ANTHROPIC_", "CLAUDE_CODE_USE_", "CLAUDE_KIRO_", "KIROCC_", "OTEL_")
RUNTIME_STRIP_NAMES = ("CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR",
                       "KIRO_API_KEY")
PROXY_VARIABLES = ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
                   "http_proxy", "https_proxy", "all_proxy")
# Isolated-profile settings this gateway cannot serve. Exact match only.
REJECTED_SETTINGS = (("model", "fable"),)


class LauncherError(Exception):
    pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


HTTP = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())


def private_directory(path):
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid():
        raise LauncherError("gateway state directory is not a directory owned by this user")
    path.chmod(0o700)


def private_file(path, flags):
    fd = os.open(path, flags | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    info = os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid():
        os.close(fd)
        raise LauncherError("gateway state file is not a regular file owned by this user")
    os.fchmod(fd, 0o600)
    return fd


@contextlib.contextmanager
def gateway_lock(path):
    # flock is released by the kernel after crashes, signals, and exec. Never
    # unlink this file: doing so allows two launchers to lock different inodes.
    # The descriptor is yielded so a spawned gateway can inherit the same open
    # file description and hold the lock through its own successful bind.
    fd = private_file(path, os.O_RDWR)
    try:
        deadline = time.monotonic() + 30
        while True:
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.monotonic() >= deadline:
                    raise LauncherError("timed out waiting for another gateway launcher")
                time.sleep(0.1)
        yield fd
    finally:
        os.close(fd)


def read_body(response):
    body = response.read(1024 * 1024)
    try:
        return json.loads(body)
    except (ValueError, UnicodeError):
        return None


def get_json(url, token=None, timeout=1):
    headers = {} if token is None else {"Authorization": "Bearer " + token}
    try:
        with HTTP.open(urllib.request.Request(url, headers=headers), timeout=timeout) as response:
            return response.status, read_body(response)
    except urllib.error.HTTPError as error:
        error.close()
        return error.code, None
    except (urllib.error.URLError, TimeoutError, OSError):
        return None, None


def post_json(url, token, headers, payload, timeout):
    request = urllib.request.Request(url, data=json.dumps(payload).encode(), method="POST")
    request.add_header("Authorization", "Bearer " + token)
    request.add_header("Content-Type", "application/json")
    for name, value in headers.items():
        request.add_header(name, value)
    try:
        with HTTP.open(request, timeout=timeout) as response:
            return response.status, read_body(response)
    except urllib.error.HTTPError as error:
        error.close()
        return error.code, None
    except (urllib.error.URLError, TimeoutError, OSError):
        return None, None


def port_is_open(port):
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=0.3):
            return True
    except OSError:
        return False


def serving_region():
    region = os.environ.get("KIRO_API_REGION") or DEFAULT_SERVING_REGION
    if region not in SERVING_REGIONS:
        raise LauncherError("KIRO_API_REGION must be one of " + ", ".join(SERVING_REGIONS))
    return region


def gateway_status(url, token):
    """Authenticated protocol identity of the service already on the port."""
    code, status = get_json(url + "/v1/kirocc/status", token, timeout=2)
    if code != 200 or not isinstance(status, dict):
        raise LauncherError("the running service does not expose the Kiro gateway status protocol; no process was stopped")
    if status.get("service") != "kirocc" or status.get("protocol_version") != STATUS_PROTOCOL_VERSION:
        raise LauncherError("the running gateway speaks a different status protocol; restart that gateway deliberately before opening new sessions")
    region = status.get("region")
    if region not in SERVING_REGIONS or status.get("inference_host") != "runtime." + region + ".kiro.dev":
        raise LauncherError("the running gateway reports an unsupported serving region; restart that gateway deliberately")
    if region != serving_region():
        raise LauncherError("the managed gateway uses a different Kiro region; restart that gateway deliberately to change regions")
    if status.get("model_mapping_mode") != "builtin":
        raise LauncherError("the running gateway serves custom model mappings and cannot be shared; restart that gateway deliberately")
    return status


def gateway_ready(url, port, token):
    """True when a compatible, authenticated Kiro gateway already serves the port.

    This answers only the serving contract. Static compatibility never attests
    upstream inference: that is established separately, exactly once, per
    gateway process.
    """
    code, health = get_json(url + "/health")
    if code is None:
        if port_is_open(port):
            raise LauncherError("the gateway port is occupied by an unresponsive or incompatible service; no process was stopped")
        return False
    if code != 200 or not isinstance(health, dict) or health.get("status") != "ok":
        raise LauncherError("the gateway port is occupied by an incompatible service; no process was stopped")
    code, models = get_json(url + "/v1/models", token)
    if code != 200:
        raise LauncherError("the running gateway rejects KIROCC_API_KEY; no process was stopped")
    if (not isinstance(models, dict) or models.get("object") != "list"
            or not isinstance(models.get("data"), list) or not models["data"]
            or not all(isinstance(model, dict) and model.get("owned_by") == "kiro"
                       for model in models["data"])):
        raise LauncherError("the running service does not advertise the Kiro gateway API")
    for probe_token in (None, "invalid-" + secrets.token_hex(16)):
        code, _ = get_json(url + "/v1/models", probe_token)
        if code != 401:
            raise LauncherError("the running gateway does not enforce local authentication; no process was stopped")
    gateway_status(url, token)
    return True


def readiness_succeeded(status):
    return status.get("upstream_probe_state") == "succeeded" and status.get("upstream_ready") is True


def probe_response_is_a_model_reply(message):
    return (isinstance(message, dict)
            and isinstance(message.get("id"), str) and bool(message["id"])
            and message.get("type") == "message"
            and message.get("role") == "assistant"
            and isinstance(message.get("content"), list))


def probe_failure(code):
    # Codes are not secrets; request and response bodies are never echoed.
    if code is None:
        return LauncherError("the readiness request to the shared gateway did not complete, so no upstream Kiro inference was established")
    if code == 403:
        return LauncherError("Kiro rejected the readiness request with HTTP 403 permission_error; the upstream subscription or API key is not entitled to this model in this region")
    return LauncherError("the shared gateway answered the readiness request with HTTP " + str(code)
                         + "; no upstream Kiro inference was established")


def probe_upstream_once(url, token):
    headers = {READINESS_HEADER: "1",
               SESSION_HEADER: "kirocc-readiness-" + secrets.token_hex(8)}
    payload = {"model": PROBE_MODEL, "max_tokens": 64, "stream": False,
               "messages": [{"role": "user", "content": "Reply exactly OK"}]}
    return post_json(url + "/v1/messages", token, headers, payload, timeout=60)


def ensure_upstream_ready(url, token):
    """Establish, exactly once per gateway process, that upstream inference works."""
    status = gateway_status(url, token)
    if readiness_succeeded(status):
        return
    if status.get("upstream_probe_state") == "unattempted":
        code, message = probe_upstream_once(url, token)
        if code == 200 and probe_response_is_a_model_reply(message):
            return
        if code != 409:
            raise probe_failure(code)
        # Another invocation won the single probe for this gateway process.
        if readiness_succeeded(gateway_status(url, token)):
            return
    # Any other state belongs to this gateway process and is not ours to retry.
    raise LauncherError("this shared gateway process has not established upstream Kiro inference;"
                        " inspect the private gateway log, then restart that gateway deliberately"
                        " before opening new sessions")


def fingerprint(path):
    with open(path, "rb") as file:
        digest = hashlib.sha256()
        for chunk in iter(lambda: file.read(1024 * 1024), b""):
            digest.update(chunk)
        return digest.hexdigest()


def process_start_identity(pid):
    try:
        result = subprocess.run(["/bin/ps", "-o", "lstart=", "-p", str(pid)],
                                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                text=True, timeout=2)
    except (OSError, subprocess.SubprocessError):
        return None
    return result.stdout.strip() or None


def save_record(path, record):
    # Informational only. This record never establishes readiness, authorizes a
    # signal, or triggers a restart, and it carries no local or upstream
    # credential. Written by atomic same-directory replacement so a reader
    # never observes a partial document.
    temporary = path.with_name(path.name + "." + secrets.token_hex(8) + ".tmp")
    try:
        fd = private_file(temporary, os.O_WRONLY | os.O_TRUNC)
        with os.fdopen(fd, "w") as file:
            json.dump(record, file)
            file.write("\n")
        os.replace(temporary, path)
    except OSError:
        with contextlib.suppress(OSError):
            os.unlink(temporary)
        raise


def credential_variables(source):
    """The gateway-only credential variables for the detected upstream auth mode."""
    key = source.get("KIRO_API_KEY")
    if key:
        if not key.startswith("ksk_") or any(character.isspace() for character in key):
            raise LauncherError("KIRO_API_KEY does not have the expected Kiro API-key format; check its credential source")
        # API-key mode never consults a credential database and never refreshes.
        return {"KIRO_API_KEY": key}
    database = source.get("KIROCC_DB_PATH")
    if not database:
        suffix = ("Library/Application Support/kiro-cli/data.sqlite3"
                  if sys.platform == "darwin" else ".local/share/kiro-cli/data.sqlite3")
        database = str(Path.home() / suffix)
    if not Path(database).is_file():
        raise LauncherError("no Kiro credential found; export KIRO_API_KEY in your shell or sign in with kiro-cli")
    return {"KIROCC_DB_PATH": database}


def gateway_environment(port, token, region):
    # Resolve the credential source from this launcher's own environment before
    # the strip, so an inherited KIROCC_DB_PATH survives in OAuth mode while the
    # rest of the KIROCC_* family cannot re-point or re-key the child. A managed
    # gateway therefore always serves the built-in model mappings.
    source = dict(os.environ)
    credentials = credential_variables(source)
    environment = {key: value for key, value in source.items()
                   if not key.startswith(GATEWAY_STRIP_PREFIXES) and key != "KIRO_API_KEY"}
    environment.update(credentials)
    environment.update(KIROCC_HOST="127.0.0.1", KIROCC_PORT=str(port),
                       KIROCC_API_KEY=token, KIRO_API_REGION=region)
    return environment


def spawn_gateway(config, port, token, region, state_dir, record_path, lock_fd):
    if not os.access(config["gateway_bin"], os.X_OK):
        raise LauncherError("the installed gateway binary is missing or not executable; rerun the installer")
    arguments = [config["gateway_bin"]]
    inherited = ()
    if lock_fd is not None:
        if lock_fd <= 2:
            raise LauncherError("the startup lock descriptor is not inheritable; rerun the launcher")
        # The child holds the same open file description and so keeps the lock
        # until it closes its copy, which it does only after a successful bind.
        # The descriptor number is not a secret.
        arguments += ["-startup-lock-fd", str(lock_fd)]
        inherited = (lock_fd,)
    environment = gateway_environment(port, token, region)
    log_path = state_dir / ("gateway-" + str(port) + ".log")
    log_fd = private_file(log_path, os.O_WRONLY | os.O_APPEND)
    with os.fdopen(log_fd, "ab", buffering=0) as log:
        process = subprocess.Popen(arguments, env=environment,
                                   stdin=subprocess.DEVNULL, stdout=log, stderr=log,
                                   start_new_session=True, close_fds=True,
                                   pass_fds=inherited)
    save_record(record_path, {"pid": process.pid,
                              "process_start_identity": process_start_identity(process.pid),
                              "gateway_sha256": fingerprint(config["gateway_bin"]),
                              "port": port,
                              "region": region,
                              "spawned_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())})
    return process, log_path


def ensure_gateway(config, port, token, state_dir, record_path, status_only, lock_fd=None):
    url = "http://127.0.0.1:" + str(port)
    if gateway_ready(url, port, token):
        if status_only:
            # --gateway-status reports the serving contract and never spends an
            # upstream inference.
            return url
        ensure_upstream_ready(url, token)
        return url
    if status_only:
        raise LauncherError("the shared gateway is not running")
    region = serving_region()
    process, log_path = spawn_gateway(config, port, token, region, state_dir, record_path, lock_fd)
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise LauncherError("gateway startup failed; inspect the private gateway log: " + str(log_path))
        if gateway_ready(url, port, token):
            ensure_upstream_ready(url, token)
            return url
        time.sleep(0.1)
    # Do not terminate any PID here: other panes may already be using the
    # gateway, and a slow start may still complete. A new invocation can retry.
    raise LauncherError("gateway readiness timed out; inspect the private gateway log: " + str(log_path))


def validate_isolated_settings(config_dir):
    """Reject unservable isolated-profile settings before any local or upstream IO."""
    path = Path(config_dir) / "settings.json"
    try:
        document = json.loads(path.read_text())
    except FileNotFoundError:
        return
    except (ValueError, UnicodeError):
        raise LauncherError("the isolated Claude settings file is not valid JSON; rerun the installer") from None
    if not isinstance(document, dict):
        raise LauncherError("the isolated Claude settings file must contain a JSON object; rerun the installer")
    for key, value in REJECTED_SETTINGS:
        if document.get(key) == value:
            raise LauncherError('the isolated Claude settings select "' + key + '": "' + value
                                + '", which this gateway cannot serve; rerun the installer to migrate it')


def runtime_environment(config, url, _caller_token=None):
    """Build the environment for the official Claude runtime.

    Everything this launcher consumes is read from its own environment before
    the strip, so no routing, telemetry, or credential variable survives into
    the runtime. The local bearer is resolved here rather than taken from the
    caller so the value handed to Claude can never be redirected by an
    intermediate argument.
    """
    source = dict(os.environ)
    local_bearer = source.get("KIROCC_API_KEY") or LOCAL_BEARER
    preserve_proxy = source.get("CLAUDE_KIRO_PRESERVE_PROXY") == "1"
    haiku = source.get("CLAUDE_KIRO_HAIKU_MODEL", "claude-haiku-4-5")
    models = {"ANTHROPIC_MODEL": source.get("CLAUDE_KIRO_MODEL", DEFAULT_SONNET_MODEL),
              "ANTHROPIC_DEFAULT_SONNET_MODEL": source.get("CLAUDE_KIRO_SONNET_MODEL", DEFAULT_SONNET_MODEL),
              "ANTHROPIC_DEFAULT_OPUS_MODEL": source.get("CLAUDE_KIRO_OPUS_MODEL", "claude-opus-4-6"),
              "ANTHROPIC_DEFAULT_HAIKU_MODEL": haiku,
              "ANTHROPIC_SMALL_FAST_MODEL": haiku}
    environment = {key: value for key, value in source.items()
                   if not key.startswith(RUNTIME_STRIP_PREFIXES)
                   and key not in RUNTIME_STRIP_NAMES}
    if not preserve_proxy:
        for key in PROXY_VARIABLES:
            environment.pop(key, None)
    environment.update(models)
    environment.update(CLAUDE_CONFIG_DIR=config["config_dir"], ANTHROPIC_BASE_URL=url,
                       ANTHROPIC_AUTH_TOKEN=local_bearer)
    return environment


def main():
    config_path = Path(__file__).with_suffix(".json")
    config = json.loads(config_path.read_text())
    config["gateway_bin"] = os.environ.get("KIROCC_BIN") or config["gateway_bin"]
    config["runtime_bin"] = os.environ.get("CLAUDE_KIRO_RUNTIME_BIN") or config["runtime_bin"]
    config["config_dir"] = (os.environ.get("CLAUDE_KIRO_CONFIG_DIR")
                            or os.environ.get("CLAWGOD_KIROCC_CONFIG_DIR") or config["config_dir"])
    config["state_root"] = os.environ.get("CLAWGOD_KIROCC_STATE_ROOT") or config["state_root"]
    validate_isolated_settings(config["config_dir"])
    try:
        port = int(os.environ.get("KIROCC_PORT") or config.get("gateway_port", 3456))
    except (ValueError, TypeError):
        raise LauncherError("KIROCC_PORT must be a number between 1 and 65535") from None
    if not 1 <= port <= 65535:
        raise LauncherError("KIROCC_PORT must be a number between 1 and 65535")
    url = "http://127.0.0.1:" + str(port)
    if os.environ.get("KIROCC_URL", url).rstrip("/") != url:
        raise LauncherError("KIROCC_URL must match http://127.0.0.1:KIROCC_PORT for the shared local gateway")
    token = os.environ.get("KIROCC_API_KEY") or LOCAL_BEARER
    if any(character in token for character in "\r\n"):
        raise LauncherError("KIROCC_API_KEY must not contain line breaks")
    args = sys.argv[1:]
    if args and args[0] == "update":
        raise LauncherError("updates are disabled in the isolated profile; rerun the project installer")
    mode = args[0] if args else ""
    if mode in ("--gateway-status", "--gateway-start") and len(args) != 1:
        raise LauncherError(mode + " does not accept additional arguments")
    state_dir = Path(config["state_root"]) / "gateway"
    # A user-wide lock coordinates profiles that share a TCP port, even when
    # their isolated Claude settings/state directories differ.
    lock_dir = Path.home() / ".cache" / "claudecode-kiro"
    private_directory(state_dir)
    private_directory(lock_dir)
    with gateway_lock(lock_dir / ("gateway-" + str(port) + ".lock")) as lock_fd:
        url = ensure_gateway(config, port, token, state_dir,
                             lock_dir / ("gateway-" + str(port) + ".json"),
                             mode == "--gateway-status", lock_fd=lock_fd)
    if mode == "--gateway-status":
        print("Shared authenticated Kiro gateway serving at " + url + " (upstream readiness not checked).")
        return
    if mode == "--gateway-start":
        print("Shared authenticated Kiro gateway ready at " + url + " with verified upstream Kiro inference.")
        return
    if not os.access(config["runtime_bin"], os.X_OK):
        raise LauncherError("the configured Claude runtime is missing or not executable; rerun the installer")
    os.execve(config["runtime_bin"], [config["runtime_bin"], *args], runtime_environment(config, url))


if __name__ == "__main__":
    try:
        main()
    except LauncherError as error:
        print("claude-kiro: " + str(error), file=sys.stderr)
        sys.exit(1)
    except KeyboardInterrupt:
        sys.exit(130)
    except (OSError, ValueError, KeyError):
        # Never print opaque exceptions that might include environment values.
        print("claude-kiro: launcher configuration or local file operation failed; rerun the installer", file=sys.stderr)
        sys.exit(1)
