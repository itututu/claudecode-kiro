#!/usr/bin/env python3
"""Expected-failing pre-fix checks for Property 1 bug conditions."""
import importlib.util
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest import mock
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


LAUNCHER_PATH = Path(__file__).with_name("claude-kiro.py")
SPEC = importlib.util.spec_from_file_location("claude_kiro_exploration", LAUNCHER_PATH)
launcher = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(launcher)


class ContractHandler(BaseHTTPRequestHandler):
    counters = None
    probe_mode = "forbidden"
    probe_state = "unattempted"

    def log_message(self, *_args):
        pass

    def _json(self, status, body):
        encoded = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)

    def do_GET(self):
        self.counters["gets"] += 1
        authenticated = self.headers.get("Authorization") == "Bearer kiro-local"
        if self.path == "/health":
            self._json(200, {"status": "ok"})
        elif self.path == "/v1/models":
            if not authenticated:
                self._json(401, {"error": "unauthorized"})
            else:
                self._json(200, {"object": "list", "data": [{"id": "claude-sonnet-4.6", "owned_by": "kiro"}]})
        elif self.path == "/v1/kirocc/status" and authenticated:
            self._json(200, {
                "service": "kirocc",
                "protocol_version": 2,
                "auth_type": "api_key",
                "region": "us-east-1",
                "inference_host": "runtime.us-east-1.kiro.dev",
                "model_mapping_mode": "builtin",
                "upstream_probe_state": self.probe_state,
                "upstream_ready": False,
                "upstream_ready_at": None,
            })
        else:
            self._json(404, {"error": "not_found"})

    def do_POST(self):
        self.counters["probes"] += 1
        type(self).probe_state = "in_progress"
        if self.probe_mode == "disconnect":
            self.connection.shutdown(socket.SHUT_RDWR)
            self.connection.close()
            return
        self._json(403, {"error": {"type": "permission_error"}})


class BugConditionExploration(unittest.TestCase):
    """Property 1: for scoped bug inputs, expectedBehavior(result) holds.

    **Validates: Requirements 1.1–1.20, 2.1–2.20**
    """

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="kiro-bug-condition-")
        self.root = Path(self.temp.name)
        self.runtime_marker = self.root / "runtime-count"
        self.runtime = self.root / "official-runtime-fixture"
        self.runtime.write_text(
            "#!/usr/bin/env python3\n"
            "import os\nfrom pathlib import Path\n"
            "p=Path(os.environ['TEST_RUNTIME_MARKER'])\n"
            "n=int(p.read_text()) if p.exists() else 0\n"
            "p.write_text(str(n+1))\n"
        )
        self.runtime.chmod(0o700)
        self.launcher = self.root / "claude-kiro.py"
        shutil.copyfile(LAUNCHER_PATH, self.launcher)
        self.launcher.chmod(0o700)
        self.counters = {"gets": 0, "probes": 0, "signals": 0}
        ContractHandler.counters = self.counters
        ContractHandler.probe_mode = "forbidden"
        ContractHandler.probe_state = "unattempted"
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), ContractHandler)
        self.port = self.server.server_address[1]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.config_dir = self.root / "config"
        self.config_dir.mkdir()
        self.state_root = self.root / "state"
        self.launcher.with_suffix(".json").write_text(json.dumps({
            "gateway_bin": "/usr/bin/false",
            "runtime_bin": str(self.runtime),
            "config_dir": str(self.config_dir),
            "state_root": str(self.state_root),
            "gateway_port": self.port,
        }))
        self.environment = {
            "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
            "HOME": str(self.root),
            "KIRO_API_KEY": "ksk_synthetic-only",
            "TEST_RUNTIME_MARKER": str(self.runtime_marker),
        }

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)
        self.temp.cleanup()

    def run_launcher(self, *arguments):
        return subprocess.run(
            [sys.executable, str(self.launcher), *arguments],
            env=self.environment,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            timeout=8,
        )

    def runtime_count(self):
        return int(self.runtime_marker.read_text()) if self.runtime_marker.exists() else 0

    def test_static_compatibility_does_not_attest_upstream_inference(self):
        result = self.run_launcher()
        observed = (result.returncode, self.counters["probes"], self.runtime_count())
        if result.returncode == 0 or self.counters["probes"] != 1 or self.runtime_count() != 0:
            self.fail(
                "counterexample: static_health_models=true messages_status=403 "
                f"returncode={observed[0]} readiness_requests={observed[1]} runtime_execs={observed[2]}"
            )

    def test_connection_loss_schedule_never_reprobes_one_process(self):
        ContractHandler.probe_mode = "disconnect"
        results = [self.run_launcher(), self.run_launcher()]
        returncodes = [result.returncode for result in results]
        if any(code == 0 for code in returncodes) or self.counters["probes"] != 1 or self.runtime_count() != 0:
            self.fail(
                "counterexample: schedule=accepted_then_connection_lost_then_second_launcher "
                f"returncodes={returncodes} readiness_requests={self.counters['probes']} "
                f"runtime_execs={self.runtime_count()}"
            )

    def test_startup_lock_ownership_is_transferable_through_bind(self):
        lock_path = self.root / "startup.lock"
        with launcher.gateway_lock(lock_path) as lock_fd:
            self.assertIsInstance(
                lock_fd, int,
                "counterexample: launcher_lock_acquired=true child_inheritable_lock_fd=false",
            )

    def test_exact_fable_setting_is_rejected_before_io(self):
        (self.config_dir / "settings.json").write_text(json.dumps({"model": "fable", "theme": "dark"}))
        result = self.run_launcher()
        if result.returncode == 0 or self.counters["gets"] != 0 or self.counters["probes"] != 0 or self.runtime_count() != 0:
            self.fail(
                "counterexample: setting=model:fable rejected_before_io=false "
                f"returncode={result.returncode} local_gets={self.counters['gets']} "
                f"readiness_requests={self.counters['probes']} runtime_execs={self.runtime_count()}"
            )

    def test_runtime_environment_rejects_routing_and_telemetry_contamination(self):
        synthetic_environment = {
            "PATH": "/usr/bin:/bin",
            "KIROCC_PORT": "4567",
            "KIROCC_MODEL_MAPPINGS": "synthetic",
            "OTEL_EXPORTER_OTLP_ENDPOINT": "synthetic",
            "CLAUDE_KIRO_MODEL": "synthetic",
            "http_proxy": "synthetic",
        }
        config = {"config_dir": str(self.config_dir)}
        with mock.patch.dict(os.environ, synthetic_environment, clear=True):
            observed = launcher.runtime_environment(config, "http://127.0.0.1:3456", "caller-selected")
        forbidden = [
            name for name in (
                "KIROCC_PORT", "KIROCC_MODEL_MAPPINGS", "OTEL_EXPORTER_OTLP_ENDPOINT",
                "CLAUDE_KIRO_MODEL", "http_proxy",
            ) if name in observed
        ]
        local_bearer_is_fixed = observed.get("ANTHROPIC_AUTH_TOKEN") == "kiro-local"
        if forbidden or not local_bearer_is_fixed:
            self.fail(
                "counterexample: runtime_environment_contamination="
                f"{forbidden} fixed_local_bearer={local_bearer_is_fixed}"
            )

    def test_stale_record_is_not_readiness_authority(self):
        config = {"gateway_bin": "/synthetic/gateway"}
        # A record naming a different PID, binary, and region is written next to
        # the live gateway. The launcher must consult the live contract only.
        record_path = self.root / "record.json"
        record_path.write_text(json.dumps(
            {"pid": 42, "gateway_sha256": "stale", "region": "wrong-region"}))
        with mock.patch.object(launcher, "gateway_ready", return_value=True), \
             mock.patch.object(launcher, "fingerprint", return_value="current"):
            try:
                launcher.ensure_gateway(config, self.port, "kiro-local", self.state_root,
                                        self.root / "record.json", True)
            except launcher.LauncherError as error:
                self.fail(f"counterexample: live_contract=compatible stale_record_overrode_readiness=true class={type(error).__name__}")

    def test_unknown_listener_receives_no_signal_or_replacement(self):
        config = {"gateway_bin": "/synthetic/gateway"}
        with mock.patch.object(launcher, "gateway_ready", side_effect=launcher.LauncherError("occupied")), \
             mock.patch.object(launcher.os, "kill") as kill, \
             mock.patch.object(launcher.subprocess, "Popen") as popen:
            with self.assertRaises(launcher.LauncherError):
                launcher.ensure_gateway(config, self.port, "kiro-local", self.state_root,
                                        self.root / "record.json", False)
        self.assertEqual(kill.call_count, 0, "unknown listener was signaled")
        self.assertEqual(popen.call_count, 0, "unknown listener was replaced")


if __name__ == "__main__":
    unittest.main(verbosity=2)
