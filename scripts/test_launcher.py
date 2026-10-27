#!/usr/bin/env python3
"""Exercise launcher behavior with local fixture processes, without Kiro traffic."""
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import unittest


GATEWAY = '''#!/usr/bin/env python3
import json, os, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
# Readiness is process-local state, exactly as the real gateway keeps it.
STATE = {'probe': 'unattempted'}
REGION = os.environ.get('KIRO_API_REGION', 'us-east-1')
class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args): pass
    def send(self, code, body):
        encoded = json.dumps(body).encode()
        self.send_response(code)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)
    def do_GET(self):
        authenticated = (self.headers.get('Authorization') == 'Bearer ' + os.environ['KIROCC_API_KEY']
                         or os.environ.get('TEST_OPEN_AUTH') == '1')
        if self.path == '/health':
            self.send(200, {'status': 'ok'})
        elif not authenticated:
            self.send(401, {'error': 'invalid API key'})
        elif self.path == '/v1/kirocc/status':
            succeeded = STATE['probe'] == 'succeeded'
            self.send(200, {'service': 'kirocc', 'protocol_version': 2, 'auth_type': 'api_key',
                            'region': REGION, 'inference_host': 'runtime.' + REGION + '.kiro.dev',
                            'model_mapping_mode': 'builtin', 'upstream_probe_state': STATE['probe'],
                            'upstream_ready': succeeded,
                            'upstream_ready_at': '2026-01-01T00:00:00Z' if succeeded else None})
        else:
            self.send(200, {'object': 'list', 'data': [{'id': 'claude-sonnet-4.6', 'owned_by': 'kiro'}]})
    def do_POST(self):
        with open(os.environ['TEST_PROBES'], 'a') as file:
            file.write(self.path + ' ' + str(self.headers.get('X-Kirocc-Readiness-Probe'))
                       + ' ' + str(bool(self.headers.get('X-Claude-Code-Session-Id'))) + '\\n')
        if STATE['probe'] != 'unattempted':
            self.send(409, {'error': {'type': 'invalid_request_error'}})
            return
        STATE['probe'] = 'succeeded'
        self.send(200, {'id': 'msg_fixture', 'type': 'message', 'role': 'assistant',
                        'content': [{'type': 'text', 'text': 'OK'}]})
server = ThreadingHTTPServer((os.environ['KIROCC_HOST'], int(os.environ['KIROCC_PORT'])), Handler)
# The inherited startup lock is released only once this copy of the descriptor
# closes, and only after a successful bind, exactly as the real gateway does.
if '-startup-lock-fd' in sys.argv:
    os.close(int(sys.argv[sys.argv.index('-startup-lock-fd') + 1]))
# Record only whether a variable arrived, never its value.
Path(os.environ['TEST_GATEWAY_ENV']).write_text(json.dumps(
    {key: key in os.environ for key in ('KIRO_API_KEY', 'KIROCC_DB_PATH', 'KIROCC_MODEL_MAPPINGS',
                                        'ANTHROPIC_BASE_URL', 'OTEL_EXPORTER_OTLP_ENDPOINT')}))
with open(os.environ['TEST_STARTED'], 'a') as file:
    file.write(str(os.getpid()) + '\\n')
server.serve_forever()
'''

RUNTIME = '''#!/usr/bin/env python3
import json, os
from pathlib import Path
# Record only synthetic test variables. Never dump the inherited environment.
keys = ['ANTHROPIC_MODEL', 'ANTHROPIC_BASE_URL', 'ANTHROPIC_AUTH_TOKEN',
        'ANTHROPIC_API_KEY', 'ANTHROPIC_NEW_PROVIDER_SWITCH', 'CLAUDE_CODE_USE_FUTURE',
        'CLAUDE_CODE_OAUTH_TOKEN', 'KIRO_API_KEY', 'KIROCC_API_KEY', 'KIROCC_DB_PATH',
        'OTEL_EXPORTER_OTLP_ENDPOINT', 'HTTP_PROXY', 'CLAUDE_CONFIG_DIR']
Path(os.environ['TEST_RUNTIME']).write_text(json.dumps({key: os.environ.get(key) for key in keys}))
print('OK')
'''


class LauncherTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='kiro-launcher-test-')
        self.root = Path(self.temp.name)
        self.launcher = self.root / 'claude-kiro.py'
        shutil.copyfile(Path(__file__).with_name('claude-kiro.py'), self.launcher)
        self.gateway = self.root / 'gateway'
        self.gateway.write_text(GATEWAY)
        self.gateway.chmod(0o700)
        self.runtime = self.root / 'claude'
        self.runtime.write_text(RUNTIME)
        self.runtime.chmod(0o700)
        self.started = self.root / 'started'
        self.probes = self.root / 'probes'
        with socket.socket() as candidate:
            candidate.bind(('127.0.0.1', 0))
            self.port = candidate.getsockname()[1]
        self.launcher.with_suffix('.json').write_text(json.dumps({
            'gateway_bin': str(self.gateway), 'runtime_bin': str(self.runtime),
            'config_dir': str(self.root / 'config'), 'state_root': str(self.root / 'state'),
            'gateway_port': self.port,
        }))
        self.env = {'PATH': os.environ['PATH'], 'HOME': str(self.root),
                    'KIRO_API_KEY': 'ksk_synthetic-test-credential',
                    'TEST_STARTED': str(self.started), 'TEST_PROBES': str(self.probes),
                    'TEST_GATEWAY_ENV': str(self.root / 'gateway-env.json'),
                    'TEST_RUNTIME': str(self.root / 'runtime.json')}

    def tearDown(self):
        if self.started.exists():
            for raw_pid in self.started.read_text().splitlines():
                try:
                    os.kill(int(raw_pid), signal.SIGTERM)
                except ProcessLookupError:
                    pass
        self.temp.cleanup()

    def run_launcher(self, *args, env=None):
        return subprocess.run([sys.executable, str(self.launcher), *args],
                              env=self.env if env is None else env, text=True,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=25)

    def pids(self):
        return self.started.read_text().splitlines() if self.started.exists() else []

    def probe_requests(self):
        return self.probes.read_text().splitlines() if self.probes.exists() else []

    def record(self):
        path = self.root / '.cache' / 'claudecode-kiro' / ('gateway-' + str(self.port) + '.json')
        return json.loads(path.read_text())

    def gateway_env(self):
        return json.loads((self.root / 'gateway-env.json').read_text())

    def test_simultaneous_sessions_share_gateway_and_survive_exit(self):
        processes = [subprocess.Popen([sys.executable, str(self.launcher)], env=self.env,
                                      text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                     for _ in range(5)]
        for process in processes:
            stdout, stderr = process.communicate(timeout=25)
            self.assertEqual(process.returncode, 0, stderr)
            self.assertEqual(stdout.strip(), 'OK')
        self.assertEqual(len(self.pids()), 1)
        result = self.run_launcher('--gateway-status')
        self.assertEqual(result.returncode, 0, result.stderr)
        os.kill(int(self.pids()[0]), 0)
        result = self.run_launcher()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(self.pids()), 1)
        # Upstream readiness is established exactly once per gateway process,
        # with the probe marker and the session header the gateway requires.
        self.assertEqual(self.probe_requests(), ['/v1/messages 1 True'])

    def test_runtime_does_not_inherit_other_provider_or_upstream_credentials(self):
        self.env.update(ANTHROPIC_MODEL='openai-apikey/incompatible', ANTHROPIC_API_KEY='synthetic',
                        ANTHROPIC_NEW_PROVIDER_SWITCH='synthetic', CLAUDE_CODE_USE_FUTURE='1',
                        CLAUDE_CODE_OAUTH_TOKEN='synthetic', HTTP_PROXY='http://invalid:1',
                        OTEL_EXPORTER_OTLP_ENDPOINT='http://invalid:1')
        result = self.run_launcher()
        self.assertEqual(result.returncode, 0, result.stderr)
        values = json.loads((self.root / 'runtime.json').read_text())
        self.assertEqual(values['ANTHROPIC_MODEL'], 'claude-sonnet-4-6')
        self.assertEqual(values['ANTHROPIC_BASE_URL'], 'http://127.0.0.1:' + str(self.port))
        self.assertEqual(values['ANTHROPIC_AUTH_TOKEN'], 'kiro-local')
        for key in ('ANTHROPIC_API_KEY', 'ANTHROPIC_NEW_PROVIDER_SWITCH', 'CLAUDE_CODE_USE_FUTURE',
                    'CLAUDE_CODE_OAUTH_TOKEN', 'KIRO_API_KEY', 'KIROCC_API_KEY', 'KIROCC_DB_PATH',
                    'OTEL_EXPORTER_OTLP_ENDPOINT', 'HTTP_PROXY'):
            self.assertIsNone(values[key], key + ' should have been removed')

    def test_gateway_receives_only_its_own_credential_and_no_routing_overrides(self):
        self.env.update(KIROCC_MODEL_MAPPINGS='[{"anthropic":"x","kiro":"y"}]',
                        ANTHROPIC_BASE_URL='http://invalid:1',
                        OTEL_EXPORTER_OTLP_ENDPOINT='http://invalid:1')
        self.assertEqual(self.run_launcher('--gateway-start').returncode, 0)
        observed = self.gateway_env()
        self.assertTrue(observed['KIRO_API_KEY'], 'the gateway needs its upstream credential')
        for key in ('KIROCC_DB_PATH', 'KIROCC_MODEL_MAPPINGS', 'ANTHROPIC_BASE_URL',
                    'OTEL_EXPORTER_OTLP_ENDPOINT'):
            self.assertFalse(observed[key], key + ' must not reach the gateway')

    def test_oauth_mode_preserves_only_the_credential_database_path(self):
        database = self.root / 'data.sqlite3'
        database.write_bytes(b'')
        env = {key: value for key, value in self.env.items() if key != 'KIRO_API_KEY'}
        env['KIROCC_DB_PATH'] = str(database)
        result = self.run_launcher('--gateway-start', env=env)
        self.assertEqual(result.returncode, 0, result.stderr)
        observed = self.gateway_env()
        self.assertTrue(observed['KIROCC_DB_PATH'], 'OAuth mode needs the credential database')
        self.assertFalse(observed['KIRO_API_KEY'], 'OAuth mode must not forge an API key')

    def test_missing_credential_is_reported_before_any_gateway_starts(self):
        env = {key: value for key, value in self.env.items() if key != 'KIRO_API_KEY'}
        result = self.run_launcher(env=env)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('no Kiro credential found', result.stderr)
        self.assertFalse(self.started.exists())

    def test_wrong_local_token_is_refused_without_stopping_gateway(self):
        first = self.run_launcher('--gateway-start')
        self.assertEqual(first.returncode, 0, first.stderr)
        result = self.run_launcher(env={**self.env, 'KIROCC_API_KEY': 'different-local-token'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('rejects KIROCC_API_KEY', result.stderr)
        self.assertEqual(len(self.pids()), 1)
        self.assertEqual(self.run_launcher('--gateway-status').returncode, 0)

    def test_unauthenticated_gateway_is_refused(self):
        result = self.run_launcher(env={**self.env, 'TEST_OPEN_AUTH': '1'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('does not enforce local authentication', result.stderr)
        self.assertFalse((self.root / 'runtime.json').exists())
        os.kill(int(self.pids()[0]), 0)

    def test_replaced_binary_is_not_grounds_for_restart_or_signal(self):
        self.assertEqual(self.run_launcher('--gateway-start').returncode, 0)
        pid = self.pids()[0]
        recorded = self.record()
        self.assertEqual(recorded['pid'], int(pid))
        self.assertEqual(recorded['port'], self.port)
        self.assertNotIn('ksk_', json.dumps(recorded))
        with self.gateway.open('a') as file:
            file.write('\n# new release\n')
        # The record is a diagnostic, not an authority: a newer installed binary
        # never justifies signaling or replacing a live, compatible gateway.
        result = self.run_launcher()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), 'OK')
        self.assertEqual(self.pids(), [pid])
        os.kill(int(pid), 0)

    def test_status_does_not_start_gateway_and_stale_lock_file_does_not_block(self):
        result = self.run_launcher('--gateway-status')
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.started.exists())
        lock_path = self.root / '.cache' / 'claudecode-kiro' / ('gateway-' + str(self.port) + '.lock')
        subprocess.run([sys.executable, '-c',
                        'import fcntl,sys; f=open(sys.argv[1],"w"); fcntl.flock(f,fcntl.LOCK_EX)',
                        str(lock_path)], check=True, timeout=5)
        result = self.run_launcher('--gateway-start')
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_status_reports_a_running_gateway_without_spending_an_inference(self):
        started = self.run_launcher('--gateway-start')
        self.assertEqual(started.returncode, 0, started.stderr)
        self.assertEqual(len(self.probe_requests()), 1)
        result = self.run_launcher('--gateway-status')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(self.probe_requests()), 1)

    def test_foreign_listener_is_never_killed(self):
        with socket.socket() as listener:
            listener.bind(('127.0.0.1', self.port))
            listener.listen()
            result = self.run_launcher()
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('occupied', result.stderr)
            self.assertFalse(self.started.exists())
            self.assertEqual(listener.getsockname()[1], self.port)

    def installer_fixture(self):
        """A PATH with a fake official runtime and Go compiler, plus install targets."""
        tools_dir = self.root / "mock tools'"
        tools_dir.mkdir()
        shutil.copyfile(self.runtime, tools_dir / 'claude')
        (tools_dir / 'claude').chmod(0o700)
        (tools_dir / 'claude.orig').write_text('#!/bin/sh\nexit 91\n')
        (tools_dir / 'claude.orig').chmod(0o700)
        fake_go = tools_dir / 'go'
        fake_go.write_text("#!/usr/bin/env python3\nimport os,shutil,sys\n"
                           "shutil.copyfile(os.environ['TEST_GATEWAY_SOURCE'], sys.argv[sys.argv.index('-o')+1])\n")
        fake_go.chmod(0o700)
        install_root = self.root / "installed runtime'"
        bin_dir = self.root / "user bin'"
        env = {**self.env, 'PATH': str(tools_dir) + os.pathsep + self.env['PATH'],
               'CLAWGOD_KIROCC_INSTALL_ROOT': str(install_root),
               'CLAWGOD_KIROCC_STATE_ROOT': str(self.root / 'state'),
               'CLAWGOD_KIROCC_BIN_DIR': str(bin_dir), 'KIROCC_PORT': str(self.port),
               'TEST_GATEWAY_SOURCE': str(self.gateway)}
        return env, tools_dir, install_root, bin_dir

    @unittest.skipUnless(shutil.which('node'), 'installer needs Node.js')
    def test_installer_writes_working_launchers_with_quoted_paths(self):
        env, tools_dir, install_root, bin_dir = self.installer_fixture()
        installer = Path(__file__).with_name('install.sh')
        result = subprocess.run(['bash', str(installer)], env=env, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=25)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((bin_dir / 'kiroclaude').is_symlink())
        installed_config = install_root / 'bin' / 'claude-kiro.json'
        self.assertEqual(json.loads(installed_config.read_text())['runtime_bin'],
                         str((tools_dir / 'claude').resolve()))
        env['CLAUDE_KIRO_RUNTIME_BIN'] = str(self.runtime)
        result = subprocess.run(['bash', str(installer)], env=env, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=25)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(installed_config.read_text())['runtime_bin'], str(self.runtime.resolve()))
        env.pop('CLAUDE_KIRO_RUNTIME_BIN')
        result = subprocess.run([str(bin_dir / 'kiroclaude')], env=env, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=25)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), 'OK')
        self.assertEqual(len(self.pids()), 1)

    @unittest.skipUnless(shutil.which('node'), 'installer needs Node.js')
    def test_installer_migrates_only_the_unservable_model_selection(self):
        env, _tools_dir, _install_root, _bin_dir = self.installer_fixture()
        installed_config = self.root / 'state' / 'claude-config' / 'settings.json'
        installed_config.parent.mkdir(parents=True)
        installed_config.write_text(json.dumps({'model': 'fable', 'theme': 'dark',
                                                'permissions': {'allow': ['WebSearch']}}))
        installer = Path(__file__).with_name('install.sh')
        result = subprocess.run(['bash', str(installer)], env=env, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=25)
        self.assertEqual(result.returncode, 0, result.stderr)
        migrated = json.loads(installed_config.read_text())
        self.assertNotIn('model', migrated)
        self.assertEqual(migrated['theme'], 'dark')
        self.assertEqual(migrated['permissions'], {'allow': ['WebSearch']})
        # Any other model selection is the user's, and installation leaves it alone.
        installed_config.write_text(json.dumps({'model': 'claude-sonnet-4-6'}))
        result = subprocess.run(['bash', str(installer)], env=env, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=25)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(installed_config.read_text()), {'model': 'claude-sonnet-4-6'})

    def test_installed_settings_selecting_an_unservable_model_are_rejected(self):
        config_dir = self.root / 'config'
        config_dir.mkdir()
        (config_dir / 'settings.json').write_text(json.dumps({'model': 'fable'}))
        result = self.run_launcher()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('select "model": "fable"', result.stderr)
        self.assertFalse(self.started.exists())

    def test_invalid_upstream_key_is_rejected_without_echoing_it(self):
        secret = 'synthetic-wrong-key-shape'
        result = self.run_launcher(env={**self.env, 'KIRO_API_KEY': secret})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('expected Kiro API-key format', result.stderr)
        self.assertNotIn(secret, result.stderr + result.stdout)
        self.assertFalse(self.started.exists())

    def test_url_override_cannot_send_local_token_elsewhere(self):
        result = self.run_launcher(env={**self.env, 'KIROCC_URL': 'https://example.invalid'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('KIROCC_URL must match', result.stderr)
        self.assertFalse(self.started.exists())

    def test_unsupported_region_is_refused_before_spawning(self):
        result = self.run_launcher(env={**self.env, 'KIRO_API_REGION': 'ap-southeast-2'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('KIRO_API_REGION must be one of', result.stderr)
        self.assertFalse(self.started.exists())


if __name__ == '__main__':
    unittest.main()
