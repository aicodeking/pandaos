"""Guest online SSH authorization regression tests; no VM/provider required."""
import base64
import contextlib
import importlib.util
import io
import http.client
import http.server
import json
import os
from pathlib import Path
import signal
import socket
import ssl
import struct
import stat
import subprocess
import tempfile
import threading
import time
import unittest
from unittest import mock

HERE = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location('ssh_authorize', HERE / 'ssh-authorize.py')
helper = importlib.util.module_from_spec(SPEC) if SPEC else None
if SPEC and SPEC.loader and (HERE / 'ssh-authorize.py').exists():
    SPEC.loader.exec_module(helper)
else:
    helper = None

KEY = base64.b64encode(struct.pack('>I', 11) + b'ssh-ed25519' + struct.pack('>I', 32) + b'k' * 32).decode()
CONFIG = dict(version=1, url='https://api.example/v1/vms/vm-1/ssh/authorize',
              token='ssh-auth-v1.test', vm_id='vm-1', team_id='team-1',
              generation=1, boot_epoch='epoch-1', users=['dev', 'user'])


class AuthorizationTests(unittest.TestCase):
    def setUp(self):
        self.assertIsNotNone(helper, 'online SSH helper is required')

    def test_config_and_digest(self):
        self.assertEqual(helper.validate_config(CONFIG), CONFIG)
        self.assertEqual(helper.config_digest(CONFIG), helper.config_digest(dict(reversed(list(CONFIG.items())))))

    def test_invalid_config_denies(self):
        for field, bad in [('version', True), ('url', 'http://api.example/x'),
                           ('url', 'https://u:p@api.example/x'), ('url', 'https://api.example/x#f'),
                           ('token', 'x\r\nInjected: y'), ('users', ['root']),
                           ('users', []), ('generation', -1), ('generation', True),
                           ('vm_id', '../bad'), ('boot_epoch', ''), ('team_id', 'bad\n')]:
            with self.subTest(field=field, bad=bad), self.assertRaises(ValueError):
                helper.validate_config(dict(CONFIG, **{field: bad}))
        with self.assertRaises(ValueError):
            helper.validate_config(dict(CONFIG, surprise=True))

    def test_exact_response_only(self):
        line = f'ssh-ed25519 {KEY}'
        self.assertEqual(helper.authorized_response(json.dumps({'authorized_key': line}).encode(), 'ssh-ed25519', KEY), line)
        for body in [b'', b'{', b'null', b'[]', b'{}', b'{"authorized_key":3}',
                     json.dumps({'authorized_key': line, 'extra': True}).encode(),
                     json.dumps({'authorized_key': line + '\n' + line}).encode(),
                     json.dumps({'authorized_key': 'restrict ' + line}).encode(),
                     json.dumps({'authorized_key': line + ' comment'}).encode(),
                     json.dumps({'authorized_key': line.replace(KEY, KEY[:-4] + 'AAAA')}).encode(),
                     b'{"authorized_key":"bad","authorized_key":' + json.dumps(line).encode() + b'}']:
            with self.subTest(body=body), self.assertRaises(ValueError):
                helper.authorized_response(body, 'ssh-ed25519', KEY)

    def test_key_validation(self):
        helper.validate_key('ssh-ed25519', KEY)
        for typ, blob in [('ssh-rsa-cert-v01@openssh.com', KEY), ('ssh-rsa', KEY),
                          ('ssh-ed25519', KEY + '\n'), ('ssh-ed25519', '%%%'),
                          ('ssh-ed25519', 'A' * 40000)]:
            with self.subTest(typ=typ, blob=blob), self.assertRaises(ValueError):
                helper.validate_key(typ, blob)

    def test_permission_checks_and_symlink(self):
        with tempfile.TemporaryDirectory() as tmp:
            p = Path(tmp) / 'config'
            p.write_text(json.dumps(CONFIG))
            p.chmod(0o640)
            with mock.patch.object(helper.os, 'getuid', return_value=0):
                # A development-owned file is never promoted to root trust.
                if os.getuid() != 0:
                    with self.assertRaises(ValueError):
                        helper.read_config(p)
            p.chmod(0o666)
            with self.assertRaises(ValueError):
                helper.read_config(p)
            link = Path(tmp) / 'link'
            link.symlink_to(p)
            with self.assertRaises((OSError, ValueError)):
                helper.read_config(link)

    def test_transport_allows_only_complete_exact_body(self):
        body = json.dumps({'authorized_key': f'ssh-ed25519 {KEY}'}).encode()
        class Reply:
            status = 200
            headers = http.client.HTTPMessage()
            headers['Content-Length'] = str(len(body))
            def getheader(self, key, default=None):
                return self.headers.get(key, default)
            def read(self, limit):
                return body
            def __enter__(self):
                return self
            def __exit__(self, *args):
                pass
        with mock.patch.object(helper.ssl, 'SSLContext'), mock.patch.object(helper.http.client, 'HTTPSConnection') as conn:
            conn.return_value.getresponse.return_value = Reply()
            conn.return_value.sock = mock.Mock()
            self.assertEqual(helper.lookup(CONFIG, 'dev', 'ssh-ed25519', KEY), f'ssh-ed25519 {KEY}')
            args, kwargs = conn.call_args
            self.assertEqual(kwargs['timeout'], 0.5)
            request = conn.return_value.request.call_args
            self.assertEqual(request.args[:2], ('POST', '/v1/vms/vm-1/ssh/authorize'))
            self.assertEqual(json.loads(request.kwargs['body']), dict(user='dev', key_type='ssh-ed25519', key_blob=KEY))
            for status in [204, 301, 302, 401, 404, 503]:
                Reply.status = status
                with self.subTest(status=status), self.assertRaises(ValueError):
                    helper.lookup(CONFIG, 'dev', 'ssh-ed25519', KEY)
            Reply.status = 200
            for bad in [body[:-1], body + b'x' * 65536]:
                with mock.patch.object(Reply, 'read', return_value=bad), self.assertRaises(ValueError):
                    helper.lookup(CONFIG, 'dev', 'ssh-ed25519', KEY)
        with mock.patch.object(helper.ssl, 'SSLContext'), mock.patch.object(helper.http.client, 'HTTPSConnection') as conn:
            with self.assertRaises(ValueError):
                helper.lookup(CONFIG, 'root', 'ssh-ed25519', KEY)
            conn.assert_not_called()

    def test_lookup_prefers_capath_and_falls_back_to_cafile_only_on_cert_error(self):
        body = json.dumps({'authorized_key': f'ssh-ed25519 {KEY}'}).encode()

        def make_reply():
            reply = mock.MagicMock()
            reply.status = 200
            hdrs = http.client.HTTPMessage()
            hdrs['Content-Length'] = str(len(body))
            reply.headers = hdrs
            reply.getheader.side_effect = lambda key, default=None: hdrs.get(key, default)
            reply.read.return_value = body
            reply.__enter__.return_value = reply
            return reply

        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            ca_dir = root / 'certs'
            ca_dir.mkdir()
            ca_file = ca_dir / 'ca-certificates.crt'
            missing_dir = root / 'missing-certs'

            # 1. Existing capath directory uses O(1) hashed lookup without touching cafile.
            verify_calls = []

            def make_ctx(*args, **kwargs):
                ctx = mock.Mock()
                ctx.load_verify_locations.side_effect = lambda **kw: verify_calls.append(kw)
                return ctx

            with mock.patch.object(helper, 'CA_DIR', ca_dir), \
                 mock.patch.object(helper, 'CA_FILE', ca_file), \
                 mock.patch.object(helper.ssl, 'SSLContext', side_effect=make_ctx), \
                 mock.patch.object(helper.http.client, 'HTTPSConnection') as conn:
                conn.return_value.getresponse.side_effect = make_reply
                self.assertEqual(helper.lookup(CONFIG, 'dev', 'ssh-ed25519', KEY), f'ssh-ed25519 {KEY}')
                self.assertEqual(verify_calls, [{'capath': str(ca_dir)}])
                self.assertEqual(conn.call_count, 1)

            # 2. Absent capath directory uses cafile directly.
            verify_calls.clear()
            with mock.patch.object(helper, 'CA_DIR', missing_dir), \
                 mock.patch.object(helper, 'CA_FILE', ca_file), \
                 mock.patch.object(helper.ssl, 'SSLContext', side_effect=make_ctx), \
                 mock.patch.object(helper.http.client, 'HTTPSConnection') as conn:
                conn.return_value.getresponse.side_effect = make_reply
                self.assertEqual(helper.lookup(CONFIG, 'dev', 'ssh-ed25519', KEY), f'ssh-ed25519 {KEY}')
                self.assertEqual(verify_calls, [{'cafile': str(ca_file)}])
                self.assertEqual(conn.call_count, 1)

            # 3. SSLCertVerificationError on capath retries once with cafile.
            verify_calls.clear()
            with mock.patch.object(helper, 'CA_DIR', ca_dir), \
                 mock.patch.object(helper, 'CA_FILE', ca_file), \
                 mock.patch.object(helper.ssl, 'SSLContext', side_effect=make_ctx), \
                 mock.patch.object(helper.http.client, 'HTTPSConnection') as conn:
                first_conn = mock.Mock()
                first_conn.request.side_effect = ssl.SSLCertVerificationError(1, 'unhashed root')
                second_conn = mock.Mock()
                second_conn.getresponse.side_effect = make_reply
                conn.side_effect = [first_conn, second_conn]
                self.assertEqual(helper.lookup(CONFIG, 'dev', 'ssh-ed25519', KEY), f'ssh-ed25519 {KEY}')
                self.assertEqual(verify_calls, [{'capath': str(ca_dir)}, {'cafile': str(ca_file)}])
                self.assertEqual(conn.call_count, 2)
                first_conn.close.assert_called_once()
                second_conn.close.assert_called_once()

            # 4. Non-certificate failures never retry with cafile.
            for err in [socket.timeout('timed out'), ConnectionRefusedError('down'), ValueError('authorization denied')]:
                verify_calls.clear()
                with self.subTest(err=err), \
                     mock.patch.object(helper, 'CA_DIR', ca_dir), \
                     mock.patch.object(helper, 'CA_FILE', ca_file), \
                     mock.patch.object(helper.ssl, 'SSLContext', side_effect=make_ctx), \
                     mock.patch.object(helper.http.client, 'HTTPSConnection') as conn:
                    conn.return_value.request.side_effect = err
                    with self.assertRaises(type(err)):
                        helper.lookup(CONFIG, 'dev', 'ssh-ed25519', KEY)
                    self.assertEqual(verify_calls, [{'capath': str(ca_dir)}])
                    self.assertEqual(conn.call_count, 1)

    def test_authorize_failure_has_empty_stdout(self):
        output = io.StringIO()
        with contextlib.redirect_stdout(output), mock.patch.object(helper, 'read_config', side_effect=ValueError('secret')):
            self.assertNotEqual(helper.main(['authorize', 'dev', 'ssh-ed25519', KEY]), 0)
        self.assertEqual(output.getvalue(), '')

    def test_hard_deadline_covers_dns(self):
        started = time.monotonic()
        with mock.patch.object(helper.ssl, 'SSLContext'), \
             mock.patch.object(helper, 'read_config', return_value=CONFIG), \
             mock.patch.object(helper.socket, 'getaddrinfo', side_effect=lambda *a, **kw: time.sleep(4)), \
             contextlib.redirect_stdout(io.StringIO()):
            self.assertNotEqual(helper.main(['authorize', 'dev', 'ssh-ed25519', KEY]), 0)
        self.assertLess(time.monotonic() - started, 2.5)

    def test_policy_excludes_legacy_and_ca(self):
        policy = helper.sshd_policy(CONFIG['users'])
        for expected in ['AuthorizedKeysFile .ssh/authorized_keys.tenant', 'TrustedUserCAKeys none',
                         'AuthorizedPrincipalsFile none', 'AuthorizedPrincipalsCommand none',
                         'AuthorizedKeysCommandUser boat-ssh', 'AuthenticationMethods publickey',
                         'HostbasedAuthentication no', 'PasswordAuthentication no', 'PermitRootLogin no']:
            self.assertIn(expected, policy)
        self.assertNotIn('Include ', policy)
        self.assertNotIn('.ssh/authorized_keys2', policy)

    def test_archive_keeps_legacy_bytes_without_enrolling(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            ssh = root / 'home' / '.ssh'
            ssh.mkdir(parents=True)
            legacy = b'unknown\x00legacy\r\n'
            (ssh / 'authorized_keys').write_bytes(legacy)
            archive = root / 'archive'
            helper.archive_legacy([ssh], archive)
            files = list(archive.iterdir())
            self.assertEqual(len(files), 1)
            self.assertEqual(files[0].read_bytes(), legacy)
            self.assertEqual(files[0].stat().st_mode & 0o777, 0o600)
            self.assertFalse((ssh / 'authorized_keys.tenant').exists())
            helper.archive_legacy([ssh], archive)
            self.assertEqual(len(list(archive.iterdir())), 1)

    def test_systemd_policy_kills_old_children(self):
        self.assertIn('KillMode=control-group', helper.SYSTEMD_POLICY)
        self.assertIn('ExecStart=/usr/sbin/sshd -D -f /etc/ssh/sshd_config', helper.SYSTEMD_POLICY)


class LiveTLSTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory(prefix='boat-ssh-tls-')
        cls.root = Path(cls.tmp.name)
        (cls.root / 'cert.conf').write_text('''[req]
distinguished_name=dn
x509_extensions=ext
prompt=no
[dn]
CN=localhost
[ext]
subjectAltName=DNS:localhost,IP:127.0.0.1
basicConstraints=critical,CA:TRUE
keyUsage=critical,digitalSignature,keyEncipherment,keyCertSign
''')
        subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes',
                        '-days', '1', '-config', str(cls.root / 'cert.conf'),
                        '-keyout', str(cls.root / 'key.pem'), '-out', str(cls.root / 'cert.pem')],
                       check=True, capture_output=True, timeout=10)

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    @contextlib.contextmanager
    def server(self, mode='allow', raw=None):
        stop = threading.Event()
        requests = []
        body = json.dumps({'authorized_key': f'ssh-ed25519 {KEY}'}).encode()
        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass
            def do_POST(self):
                requests.append((self.path, dict(self.headers), self.rfile.read(int(self.headers['Content-Length']))))
                try:
                    if raw is not None:
                        self.wfile.write(raw)
                        return
                    self.send_response(302 if mode == 'redirect' else 200)
                    if mode == 'redirect':
                        self.send_header('Location', '/elsewhere')
                    if mode == 'slow':
                        self.send_header('Content-Length', str(len(body) + 100))
                        self.end_headers()
                        self.wfile.write(body)
                        self.wfile.flush()
                        for _ in range(100):
                            if stop.wait(0.1):
                                break
                            self.wfile.write(b' ')
                            self.wfile.flush()
                        return
                    content = body[:-2] if mode == 'partial' else body
                    if mode == 'oversized':
                        content = b'x' * 65537
                    self.send_header('Content-Length', str(len(body) if mode == 'partial' else len(content)))
                    self.end_headers()
                    self.wfile.write(content)
                except (BrokenPipeError, ConnectionResetError, ssl.SSLError):
                    pass
        srv = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        srv.daemon_threads = False
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(self.root / 'cert.pem', self.root / 'key.pem')
        srv.socket = context.wrap_socket(srv.socket, server_side=True)
        thread = threading.Thread(target=srv.serve_forever)
        thread.start()
        try:
            yield dict(CONFIG, url=f'https://127.0.0.1:{srv.server_port}/v1/vms/vm-1/ssh/authorize'), requests
        finally:
            stop.set()
            srv.shutdown()
            srv.server_close()
            thread.join(timeout=3)
            self.assertFalse(thread.is_alive())

    @contextlib.contextmanager
    def local_ca(self, trusted=True):
        original = ssl.SSLContext.load_verify_locations
        cert = str(self.root / 'cert.pem')
        def load(context, *args, **kwargs):
            if trusted:
                return original(context, cafile=cert)
        with mock.patch.object(ssl.SSLContext, 'load_verify_locations', load):
            yield

    def test_real_tls_allow_ignores_proxy_environment(self):
        with self.server() as (config, requests), self.local_ca(), \
             mock.patch.dict(os.environ, {'HTTPS_PROXY': 'http://127.0.0.1:1',
                                          'SSL_CERT_FILE': '/untrusted', 'SSL_CERT_DIR': '/untrusted'}):
            self.assertEqual(helper.lookup(config, 'dev', 'ssh-ed25519', KEY), f'ssh-ed25519 {KEY}')
            self.assertEqual(len(requests), 1)
            self.assertEqual(requests[0][1]['Authorization'], 'Bearer ' + CONFIG['token'])

    def test_real_tls_rejects_untrusted_certificate(self):
        with self.server() as (config, requests), self.local_ca(trusted=False):
            with self.assertRaises(ssl.SSLCertVerificationError):
                helper.lookup(config, 'dev', 'ssh-ed25519', KEY)
            self.assertEqual(requests, [])

    def test_real_tls_capath_hash_lookup_and_cafile_fallback(self):
        with tempfile.TemporaryDirectory(prefix='boat-capath-') as tmp:
            root = Path(tmp)
            hashed_dir = root / 'hashed'
            empty_dir = root / 'empty'
            hashed_dir.mkdir()
            empty_dir.mkdir()
            cert_pem = self.root / 'cert.pem'
            for flag in ('-subject_hash', '-subject_hash_old'):
                res = subprocess.run(['openssl', 'x509', flag, '-noout', '-in', str(cert_pem)],
                                     check=True, capture_output=True, text=True, timeout=10)
                link = hashed_dir / (res.stdout.strip() + '.0')
                if not link.exists():
                    link.symlink_to(cert_pem)

            # Fast path: hashed capath succeeds without cafile, ignoring SSL_CERT_* env vars.
            with self.server() as (config, requests), \
                 mock.patch.object(helper, 'CA_DIR', hashed_dir), \
                 mock.patch.object(helper, 'CA_FILE', root / 'missing-ca.crt'), \
                 mock.patch.dict(os.environ, {'SSL_CERT_FILE': '/untrusted', 'SSL_CERT_DIR': '/untrusted'}):
                self.assertEqual(helper.lookup(config, 'dev', 'ssh-ed25519', KEY), f'ssh-ed25519 {KEY}')
                self.assertEqual(len(requests), 1)

            # Fallback path: unhashed custom root present only in ca-certificates.crt succeeds on retry.
            with self.server() as (config, requests), \
                 mock.patch.object(helper, 'CA_DIR', empty_dir), \
                 mock.patch.object(helper, 'CA_FILE', cert_pem), \
                 mock.patch.dict(os.environ, {'SSL_CERT_FILE': '/untrusted', 'SSL_CERT_DIR': '/untrusted'}):
                self.assertEqual(helper.lookup(config, 'dev', 'ssh-ed25519', KEY), f'ssh-ed25519 {KEY}')
                self.assertEqual(len(requests), 1)

            # Untrusted in both capath and cafile still fails closed with SSLCertVerificationError.
            other_ca = root / 'other-ca.pem'
            subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes',
                            '-days', '1', '-subj', '/CN=other-ca',
                            '-keyout', str(root / 'other.key'), '-out', str(other_ca)],
                           check=True, capture_output=True, timeout=10)
            with self.server() as (config, requests), \
                 mock.patch.object(helper, 'CA_DIR', empty_dir), \
                 mock.patch.object(helper, 'CA_FILE', other_ca):
                with self.assertRaises(ssl.SSLCertVerificationError):
                    helper.lookup(config, 'dev', 'ssh-ed25519', KEY)
                self.assertEqual(requests, [])

    def test_real_tls_redirect_partial_and_oversize_deny(self):
        for mode in ['redirect', 'partial', 'oversized']:
            with self.subTest(mode=mode), self.server(mode) as (config, requests), self.local_ca():
                output = io.StringIO()
                with mock.patch.object(helper, 'read_config', return_value=config), contextlib.redirect_stdout(output):
                    self.assertEqual(helper.main(['authorize', 'dev', 'ssh-ed25519', KEY]), 1)
                self.assertEqual(output.getvalue(), '')
                self.assertEqual(len(requests), 1)

    def test_real_tls_raw_response_framing(self):
        body = json.dumps({'authorized_key': f'ssh-ed25519 {KEY}'}).encode()
        length = f'Content-Length: {len(body)}\r\n'.encode()
        chunked = f'{len(body):x}\r\n'.encode() + body + b'\r\n0\r\n\r\n'
        cases = [
            ('valid', length, body, True),
            ('valid-at-limit', f'Content-Length: {helper.LIMIT}\r\n'.encode(),
             body + b' ' * (helper.LIMIT - len(body)), True),
            ('missing-length', b'', body, False),
            ('duplicate-length', length * 2, body, False),
            ('conflicting-length', length + b'Content-Length: 1\r\n', body, False),
            ('comma-length', b'Content-Length: 102, 102\r\n', body, False),
            ('folded-length', length + b' 102\r\n', body, False),
            ('invalid-header', length + b'bad-header\r\n', body, False),
            ('invalid-header-name', length + b'Transfer-Encoding : gzip\r\n', body, False),
            ('short-body', f'Content-Length: {len(body) + 1}\r\n'.encode(), body, False),
            ('trailing-byte', length, body + b'x', False),
            ('trailing-whitespace', length, body + b'\n', False),
            ('trailing-response', length, body + b'HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n', False),
            ('oversized-declaration', f'Content-Length: {helper.LIMIT + 1}\r\n'.encode(), body, False),
            ('oversized-body', length, body + b' ' * helper.LIMIT, False),
        ]
        for invalid in [b'', b'-1', b'+102', b'102.0', b'102x']:
            cases.append(('invalid-length-' + repr(invalid), b'Content-Length: ' + invalid + b'\r\n', body, False))
        for encoding, content in [(b'chunked', chunked), (b'gzip', body), (b'', body)]:
            transfer = b'Transfer-Encoding: ' + encoding + b'\r\n'
            for framing in [transfer, transfer + length, transfer * 2 + length]:
                cases.append(('transfer-' + repr(framing), framing, content, False))
        for name, headers, content, allowed in cases:
            raw = b'HTTP/1.1 200 OK\r\n' + headers + b'Connection: close\r\n\r\n' + content
            with self.subTest(case=name), self.server(raw=raw) as (config, requests), self.local_ca():
                output = io.StringIO()
                with mock.patch.object(helper, 'read_config', return_value=config), contextlib.redirect_stdout(output):
                    status = helper.main(['authorize', 'dev', 'ssh-ed25519', KEY])
                self.assertEqual(status, 0 if allowed else 1)
                self.assertEqual(output.getvalue(), f'ssh-ed25519 {KEY}\n' if allowed else '')
                self.assertEqual(len(requests), 1)

    def test_real_tls_slow_declared_body_hits_total_deadline(self):
        with self.server('slow') as (config, requests), self.local_ca():
            output = io.StringIO()
            started = time.monotonic()
            with mock.patch.object(helper, 'read_config', return_value=config), contextlib.redirect_stdout(output):
                self.assertEqual(helper.main(['authorize', 'dev', 'ssh-ed25519', KEY]), 1)
            elapsed = time.monotonic() - started
            self.assertGreater(elapsed, 1.8)
            self.assertLess(elapsed, 2.5)
            self.assertEqual(output.getvalue(), '')


class LocalSshdTests(unittest.TestCase):
    def test_actual_sshd_validates_effective_policy_for_both_aliases(self):
        if not Path('/usr/sbin/sshd').exists():
            self.skipTest('local sshd unavailable')
        with tempfile.TemporaryDirectory(prefix='boat-sshd-policy-') as tmp:
            root = Path(tmp)
            subprocess.run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(root / 'host')],
                           check=True, capture_output=True, timeout=10)
            import pwd
            current = pwd.getpwuid(os.getuid()).pw_name
            # Substitute only host-specific paths/account; no host config changes.
            policy = helper.sshd_policy(CONFIG['users'])
            policy = policy.replace('AuthorizedKeysCommandUser boat-ssh', 'AuthorizedKeysCommandUser ' + current)
            policy = policy.replace('HostKey /etc/ssh/ssh_host_ed25519_key', 'HostKey ' + str(root / 'host'))
            policy = policy.replace('HostKey /etc/ssh/ssh_host_ecdsa_key\n', '')
            (root / 'sshd_config').write_text(policy)
            subprocess.run(['/usr/sbin/sshd', '-t', '-f', str(root / 'sshd_config')],
                           check=True, capture_output=True, timeout=10)
            for user in ['dev', 'user']:
                result = subprocess.run(['/usr/sbin/sshd', '-T', '-f', str(root / 'sshd_config'),
                                         '-C', f'user={user},host=localhost,addr=127.0.0.1'],
                                        check=True, capture_output=True, text=True, timeout=10)
                effective = dict(line.split(' ', 1) for line in result.stdout.splitlines() if ' ' in line)
                self.assertEqual(effective['authorizedkeysfile'], '.ssh/authorized_keys.tenant')
                self.assertEqual(effective['trustedusercakeys'], 'none')
                self.assertEqual(effective['authorizedkeyscommand'], '/usr/bin/python3 -I /usr/local/libexec/boat-ssh-authorize authorize %u %t %k')
                self.assertEqual([line for line in result.stdout.splitlines() if line.startswith('allowusers ')], ['allowusers dev', 'allowusers user'])


class InstallerBoundaryTests(unittest.TestCase):
    def test_publish_occurs_after_deadline_is_disabled(self):
        class Output(io.StringIO):
            def write(self, value):
                if signal.getitimer(signal.ITIMER_REAL)[0] != 0:
                    raise AssertionError('output under active deadline')
                return super().write(value)
        output = Output()
        with mock.patch.object(helper, 'read_config', return_value=CONFIG), \
             mock.patch.object(helper, 'lookup', return_value=f'ssh-ed25519 {KEY}'), \
             contextlib.redirect_stdout(output):
            self.assertEqual(helper.main(['authorize', 'dev', 'ssh-ed25519', KEY]), 0)
        self.assertEqual(output.getvalue(), f'ssh-ed25519 {KEY}\n')

    def test_verify_rejects_extra_service_authorization_options(self):
        def command(*args, **kwargs):
            if 'show' in args:
                return subprocess.CompletedProcess(args, 0,
                    'KillMode=control-group\nExecStart={ path=/usr/sbin/sshd ; argv[]=/usr/sbin/sshd -D -f /etc/ssh/sshd_config -o AuthorizedKeysFile=/tmp/legacy ; ignore_errors=no ; }\n', '')
            return subprocess.CompletedProcess(args, 3 if 'ssh.socket' in args else 0, '', '')
        with mock.patch.object(helper.os, 'geteuid', return_value=0), \
             mock.patch.object(helper, 'verify_policy'), mock.patch.object(helper, 'run', command), \
             self.assertRaises(ValueError):
            helper.verify(CONFIG)

    def test_stop_failure_never_writes_or_starts_policy(self):
        with mock.patch.object(helper.os, 'geteuid', return_value=0), \
             mock.patch.object(helper, 'stop_old_sshd', side_effect=ValueError('cannot stop')), \
             mock.patch.object(helper, 'atomic_file') as write, \
             mock.patch.object(helper, 'run') as command, self.assertRaises(ValueError):
            helper.install(CONFIG)
        write.assert_not_called()
        command.assert_not_called()

    def test_nonroot_install_and_verify_deny(self):
        with mock.patch.object(helper.os, 'geteuid', return_value=1000):
            for op in [helper.install, helper.verify, helper.rotate_token, helper.install_and_verify]:
                with self.assertRaises(ValueError):
                    op(CONFIG)

    def test_install_and_verify_fast_path_and_helper_sha256_guards(self):
        import hashlib
        from types import SimpleNamespace
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            helper_path = root / 'usr' / 'local' / 'libexec' / 'boat-ssh-authorize'
            helper_path.parent.mkdir(parents=True, exist_ok=True)
            helper_bytes = (HERE / 'ssh-authorize.py').read_bytes()
            helper_sha = hashlib.sha256(helper_bytes).hexdigest()
            helper_path.write_bytes(helper_bytes)
            helper_path.chmod(0o555)

            real_fstat = os.fstat
            real_lstat = Path.lstat
            uid_override = [0]
            gid_override = [0]

            def fake_fstat(fd):
                st = real_fstat(fd)
                return SimpleNamespace(st_mode=st.st_mode, st_uid=uid_override[0], st_gid=gid_override[0], st_size=st.st_size)

            def fake_lstat(path_self):
                st = real_lstat(path_self)
                return SimpleNamespace(st_mode=st.st_mode, st_uid=uid_override[0], st_gid=gid_override[0], st_size=st.st_size)

            def invoke(argv, payload=None):
                raw_in = helper.canonical(CONFIG) if payload is None else payload
                stdin_obj = SimpleNamespace(buffer=io.BytesIO(raw_in))
                out = io.StringIO()
                with mock.patch.object(helper.sys, 'stdin', stdin_obj), contextlib.redirect_stdout(out):
                    rc = helper.main(argv)
                return rc, out.getvalue()

            with mock.patch.object(helper, 'HELPER_PATH', helper_path), \
                 mock.patch.object(helper, 'protected_parent'), \
                 mock.patch.object(helper.os, 'geteuid', return_value=0), \
                 mock.patch.object(helper.os, 'fstat', side_effect=fake_fstat), \
                 mock.patch.object(Path, 'lstat', fake_lstat):

                # 1. Matching --expect-helper-sha256 runs install + verify in order and prints single proof JSON.
                order = []
                with mock.patch.object(helper, 'install', side_effect=lambda c: order.append(('install', c))), \
                     mock.patch.object(helper, 'verify', side_effect=lambda c: order.append(('verify', c))):
                    rc, stdout = invoke(['install-and-verify', '--expect-helper-sha256', helper_sha])
                self.assertEqual(rc, 0)
                self.assertEqual(order, [('install', CONFIG), ('verify', CONFIG)])
                proof = json.loads(stdout)
                self.assertEqual(proof, {
                    'version': 1,
                    'config_digest': helper.config_digest(CONFIG),
                    'boot_epoch': CONFIG['boot_epoch'],
                    'readiness': True,
                })

                # 2. Without --expect-helper-sha256 also runs install + verify and prints single proof JSON.
                order.clear()
                with mock.patch.object(helper, 'install', side_effect=lambda c: order.append(('install', c))), \
                     mock.patch.object(helper, 'verify', side_effect=lambda c: order.append(('verify', c))):
                    rc, stdout = invoke(['install-and-verify'])
                self.assertEqual(rc, 0)
                self.assertEqual(order, [('install', CONFIG), ('verify', CONFIG)])
                self.assertEqual(json.loads(stdout)['config_digest'], helper.config_digest(CONFIG))

                # 3. Verify failure inside install-and-verify exits non-zero with empty stdout.
                with mock.patch.object(helper, 'install'), \
                     mock.patch.object(helper, 'verify', side_effect=ValueError('sshd -T drift')):
                    rc, stdout = invoke(['install-and-verify', '--expect-helper-sha256', helper_sha])
                self.assertEqual(rc, 1)
                self.assertEqual(stdout, '')

                # 4. Mismatched/malformed sha256 fails closed BEFORE touching install or verify.
                for bad_sha in ['0' * 64, 'A' * 64, 'short', '']:
                    with self.subTest(bad_sha=bad_sha), \
                         mock.patch.object(helper, 'install') as inst_mock, \
                         mock.patch.object(helper, 'verify') as ver_mock:
                        rc, stdout = invoke(['install-and-verify', '--expect-helper-sha256', bad_sha])
                        self.assertEqual(rc, 1)
                        self.assertEqual(stdout, '')
                        inst_mock.assert_not_called()
                        ver_mock.assert_not_called()

                # 5. Writable mode, non-root owner/group, symlink, or missing helper fails closed BEFORE install.
                for bad_mode in [0o755, 0o575, 0o557, 0o644]:
                    helper_path.chmod(bad_mode)
                    with self.subTest(bad_mode=oct(bad_mode)), \
                         mock.patch.object(helper, 'install') as inst_mock:
                        rc, stdout = invoke(['install-and-verify', '--expect-helper-sha256', helper_sha])
                        self.assertEqual(rc, 1)
                        self.assertEqual(stdout, '')
                        inst_mock.assert_not_called()
                helper_path.chmod(0o555)

                for bad_uid, bad_gid in [(1000, 0), (0, 42)]:
                    uid_override[0], gid_override[0] = bad_uid, bad_gid
                    with self.subTest(uid=bad_uid, gid=bad_gid), \
                         mock.patch.object(helper, 'install') as inst_mock:
                        rc, stdout = invoke(['install-and-verify', '--expect-helper-sha256', helper_sha])
                        self.assertEqual(rc, 1)
                        self.assertEqual(stdout, '')
                        inst_mock.assert_not_called()
                uid_override[0], gid_override[0] = 0, 0

                target = root / 'target-helper'
                target.write_bytes(helper_bytes)
                target.chmod(0o555)
                helper_path.unlink()
                helper_path.symlink_to(target)
                with mock.patch.object(helper, 'install') as inst_mock:
                    rc, stdout = invoke(['install-and-verify', '--expect-helper-sha256', helper_sha])
                    self.assertEqual(rc, 1)
                    self.assertEqual(stdout, '')
                    inst_mock.assert_not_called()

                helper_path.unlink()
                with mock.patch.object(helper, 'install') as inst_mock:
                    rc, stdout = invoke(['install-and-verify', '--expect-helper-sha256', helper_sha])
                    self.assertEqual(rc, 1)
                    self.assertEqual(stdout, '')
                    inst_mock.assert_not_called()

    def test_config_mode_owner_group_checks(self):
        from types import SimpleNamespace
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'config'
            path.write_text(json.dumps(CONFIG))
            metadata = dict(st_mode=stat.S_IFREG | 0o640, st_uid=0, st_gid=42)
            with mock.patch.object(helper, 'protected_parent'), \
                 mock.patch.object(helper.grp, 'getgrnam', return_value=SimpleNamespace(gr_gid=42)), \
                 mock.patch.object(helper.os, 'fstat', return_value=SimpleNamespace(**metadata)):
                self.assertEqual(helper.read_config(path), CONFIG)
            for changes in [dict(st_uid=1000), dict(st_gid=0), dict(st_mode=stat.S_IFREG | 0o644),
                            dict(st_mode=stat.S_IFREG | 0o660), dict(st_mode=stat.S_IFIFO | 0o640)]:
                with self.subTest(changes=changes), mock.patch.object(helper, 'protected_parent'), \
                     mock.patch.object(helper.grp, 'getgrnam', return_value=SimpleNamespace(gr_gid=42)), \
                     mock.patch.object(helper.os, 'fstat', return_value=SimpleNamespace(**dict(metadata, **changes))), \
                     self.assertRaises(ValueError):
                    helper.read_config(path)

    def test_install_skips_unchanged_sshd_config_dropin_and_daemon_reload(self):
        from types import SimpleNamespace
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cfg_path = root / 'etc' / 'boat' / 'ssh-auth.json'
            archive_path = root / 'etc' / 'boat' / 'ssh-legacy-archive'
            sshd_cfg = root / 'etc' / 'ssh' / 'sshd_config'
            dropin_path = root / 'etc' / 'systemd' / 'system' / 'ssh.service.d' / 'boat-authority.conf'
            helper_path = root / 'usr' / 'local' / 'libexec' / 'boat-ssh-authorize'
            run_dir = root / 'run' / 'sshd'
            sshd_bin = root / 'usr' / 'sbin' / 'sshd'
            home_dev = root / 'home' / 'dev'
            for d in [cfg_path.parent, sshd_cfg.parent, dropin_path.parent, helper_path.parent,
                      run_dir.parent, sshd_bin.parent, home_dev / '.ssh']:
                d.mkdir(parents=True, exist_ok=True)
            sshd_cfg.write_bytes(helper.sshd_policy(CONFIG['users']).encode())
            sshd_cfg.chmod(0o644)
            dropin_path.write_bytes(helper.SYSTEMD_POLICY.encode())
            dropin_path.chmod(0o644)
            helper_path.write_bytes(b'#!/usr/bin/python3 -I\n')
            helper_path.chmod(0o555)
            sshd_bin.write_bytes(b'')
            sshd_bin.chmod(0o755)

            host_keys = tuple(sshd_cfg.parent / ('ssh_host_' + typ + '_key')
                              for typ in ('ed25519', 'ecdsa'))
            host_keys[0].write_bytes(b'')
            host_keys[0].with_suffix('.pub').write_bytes(b'stale')
            commands = []
            writes = []

            def fake_run(*argv, check=True):
                commands.append(argv)
                if argv[0] == '/usr/bin/ssh-keygen':
                    key = Path(argv[-1])
                    self.assertFalse(key.exists())
                    self.assertFalse(key.with_suffix('.pub').exists())
                    key.write_bytes(b'private stub')
                    key.with_suffix('.pub').write_bytes(b'public stub')
                if 'show' in argv:
                    return subprocess.CompletedProcess(
                        argv, 0,
                        'KillMode=control-group\nExecStart={ path=/usr/sbin/sshd ; argv[]=/usr/sbin/sshd -D -f /etc/ssh/sshd_config ; }\n',
                        '')
                return subprocess.CompletedProcess(argv, 3 if 'ssh.socket' in argv else 0, '', '')

            def fake_atomic(path, data, mode, gid=0):
                path = Path(path)
                writes.append(path)
                path.write_bytes(data)
                path.chmod(mode)

            real_fstat = os.fstat
            real_lstat = Path.lstat

            def root_fstat(fd):
                st = real_fstat(fd)
                gid = 42 if cfg_path.exists() and st.st_ino == real_lstat(cfg_path).st_ino else 0
                return SimpleNamespace(st_mode=st.st_mode, st_uid=0, st_gid=gid, st_size=st.st_size)

            def root_lstat(path_self):
                st = real_lstat(path_self)
                return SimpleNamespace(st_mode=st.st_mode, st_uid=0, st_gid=0, st_size=st.st_size)

            def fake_pwnam(name):
                if name == 'boat-ssh':
                    return SimpleNamespace(pw_uid=999, pw_gid=42, pw_shell='/usr/sbin/nologin', pw_dir='/nonexistent')
                return SimpleNamespace(pw_uid=1000, pw_gid=1000, pw_shell='/bin/bash', pw_dir=str(home_dev))

            with mock.patch.object(helper, 'CONFIG_PATH', cfg_path), \
                 mock.patch.object(helper, 'ARCHIVE', archive_path), \
                 mock.patch.object(helper, 'SSHD_CONFIG', sshd_cfg), \
                 mock.patch.object(helper, 'DROPIN_PATH', dropin_path), \
                 mock.patch.object(helper, 'HELPER_PATH', helper_path), \
                 mock.patch.object(helper, 'SSHD_RUN_DIR', run_dir), \
                 mock.patch.object(helper, 'SSHD_BIN', sshd_bin), \
                 mock.patch.object(helper, 'HOST_KEYS', host_keys), \
                 mock.patch.object(helper.os, 'geteuid', return_value=0), \
                 mock.patch.object(helper.os, 'fstat', side_effect=root_fstat), \
                 mock.patch.object(Path, 'lstat', root_lstat), \
                 mock.patch.object(helper, 'protected_parent'), \
                 mock.patch.object(helper, 'stop_old_sshd'), \
                 mock.patch.object(helper.pwd, 'getpwnam', side_effect=fake_pwnam), \
                 mock.patch.object(helper.grp, 'getgrnam', return_value=SimpleNamespace(gr_gid=42)), \
                 mock.patch.object(helper, 'atomic_file', side_effect=fake_atomic), \
                 mock.patch.object(helper, 'run', side_effect=fake_run):
                helper.install(CONFIG)

                self.assertEqual(writes, [cfg_path])
                self.assertEqual(len([c for c in commands if c[0] == '/usr/bin/ssh-keygen']), 2)
                reset = ('/usr/bin/systemctl', 'reset-failed', 'ssh.service')
                start = ('/usr/bin/systemctl', 'start', 'ssh.service')
                self.assertLess(commands.index(reset), commands.index(start))
                for key in host_keys:
                    self.assertEqual(key.read_bytes(), b'private stub')
                    self.assertEqual(stat.S_IMODE(key.stat().st_mode), 0o600)
                    self.assertEqual(stat.S_IMODE(key.with_suffix('.pub').stat().st_mode), 0o644)
                self.assertTrue(run_dir.is_dir())
                self.assertEqual(stat.S_IMODE(real_lstat(run_dir).st_mode), 0o755)
                self.assertNotIn(('/usr/bin/systemctl', 'daemon-reload'), commands)
                self.assertIn((str(sshd_bin), '-t', '-f', str(sshd_cfg)), commands)
                self.assertFalse(any('-T' in argv for argv in commands))

                # Changing or mis-moding the drop-in or sshd_config triggers rewrite + daemon-reload.
                commands.clear()
                writes.clear()
                dropin_path.write_bytes(b'[Service]\nExecStart=/bin/false\n')
                sshd_cfg.chmod(0o600)
                helper.install(CONFIG)
                self.assertIn(sshd_cfg, writes)
                self.assertIn(dropin_path, writes)
                self.assertIn(cfg_path, writes)
                self.assertIn(('/usr/bin/systemctl', 'daemon-reload'), commands)

                # Symlinked drop-in is rejected by verify_files_and_config if tampered.
                tampered = root / 'tampered'
                tampered.write_bytes(helper.SYSTEMD_POLICY.encode())
                dropin_path.unlink()
                dropin_path.symlink_to(tampered)
                with self.assertRaises((OSError, ValueError)):
                    helper.verify_files_and_config(CONFIG)

    def test_install_and_verify_skips_restart_when_unchanged(self):
        with mock.patch.object(helper.os, 'geteuid', return_value=0), \
             mock.patch.object(helper, 'sshd_unchanged', return_value=True), \
             mock.patch.object(helper, 'refresh_config_only') as refresh, \
             mock.patch.object(helper, 'verify_policy') as policy, \
             mock.patch.object(helper, 'install') as install, \
             mock.patch.object(helper, 'verify') as verify:
            helper.install_and_verify(CONFIG)
        refresh.assert_called_once_with(CONFIG)
        policy.assert_called_once_with(CONFIG)
        install.assert_not_called()
        verify.assert_not_called()

    def test_install_and_verify_restarts_when_changed(self):
        with mock.patch.object(helper.os, 'geteuid', return_value=0), \
             mock.patch.object(helper, 'sshd_unchanged', return_value=False), \
             mock.patch.object(helper, 'refresh_config_only') as refresh, \
             mock.patch.object(helper, 'install') as install, \
             mock.patch.object(helper, 'verify') as verify:
            helper.install_and_verify(CONFIG)
        install.assert_called_once_with(CONFIG)
        verify.assert_called_once_with(CONFIG)
        refresh.assert_not_called()

    def test_sshd_unchanged_requires_files_current_and_sshd_newer(self):
        from types import SimpleNamespace
        fake_stat = SimpleNamespace(st_mode=stat.S_IFREG | 0o555, st_uid=0, st_gid=0, st_mtime=1000.0, st_size=1)
        show = subprocess.CompletedProcess((), 0, '123\n', '')
        def check(files_ok=True, started=2000.0, service_ok=True, pid_out=show):
            service = mock.Mock() if service_ok else mock.Mock(side_effect=ValueError('inactive'))
            with mock.patch.object(helper, 'check_accounts', return_value=([], 42)), \
                 mock.patch.object(helper, 'protected_parent'), \
                 mock.patch.object(helper.Path, 'lstat', return_value=fake_stat), \
                 mock.patch.object(helper, 'exact_root_file', return_value=files_ok), \
                 mock.patch.object(helper, 'verify_service', service), \
                 mock.patch.object(helper, 'run', return_value=pid_out), \
                 mock.patch.object(helper, 'process_start_time', return_value=started):
                return helper.sshd_unchanged(CONFIG)
        self.assertTrue(check())
        fake_stat.st_size = 0
        self.assertFalse(check())
        fake_stat.st_size = 1
        self.assertFalse(check(files_ok=False))
        self.assertFalse(check(started=500.0))      # sshd predates config: restart
        self.assertFalse(check(service_ok=False))
        self.assertFalse(check(pid_out=subprocess.CompletedProcess((), 0, '0\n', '')))

    def test_verify_service_single_systemctl_show_stanzas(self):
        good_out = (
            'Id=ssh.service\n'
            'ActiveState=active\n'
            'KillMode=control-group\n'
            'ExecStart={ path=/usr/sbin/sshd ; argv[]=/usr/sbin/sshd -D -f /etc/ssh/sshd_config ; ignore_errors=no ; }\n'
            '\n'
            'Id=ssh.socket\n'
            'ActiveState=inactive\n'
            'KillMode=control-group\n'
        )
        calls = []

        def fake_show(stdout):
            def _run(*argv, check=True):
                calls.append(argv)
                return subprocess.CompletedProcess(argv, 0, stdout, '')
            return _run

        with mock.patch.object(helper, 'run', side_effect=fake_show(good_out)):
            helper.verify_service()
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0][:4], ('/usr/bin/systemctl', 'show', 'ssh.service', 'ssh.socket'))

        bad_outputs = [
            good_out.replace('Id=ssh.socket\nActiveState=inactive', 'Id=ssh.socket\nActiveState=active'),
            good_out.replace('Id=ssh.socket\nActiveState=inactive', 'Id=ssh.socket\nActiveState=activating'),
            good_out.replace('Id=ssh.service\nActiveState=active', 'Id=ssh.service\nActiveState=inactive'),
            good_out.replace('Id=ssh.service\nActiveState=active', 'Id=ssh.service\nActiveState=failed'),
            good_out.replace('KillMode=control-group', 'KillMode=process', 1),
            good_out.replace('/etc/ssh/sshd_config', '/etc/ssh/sshd_config -o PermitRootLogin=yes'),
        ]
        for bad in bad_outputs:
            calls.clear()
            with self.subTest(bad=bad), mock.patch.object(helper, 'run', side_effect=fake_show(bad)), \
                 self.assertRaises(ValueError):
                helper.verify_service()
            self.assertEqual(len(calls), 1)

    def test_stop_old_sshd_skips_mask_when_socket_already_masked(self):
        from types import SimpleNamespace
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            etc_sock = root / 'etc' / 'ssh.socket'
            run_sock = root / 'run' / 'ssh.socket'
            etc_sock.parent.mkdir(parents=True)
            run_sock.parent.mkdir(parents=True)
            etc_sock.symlink_to('/dev/null')

            real_lstat = Path.lstat

            def root_lstat(path_self):
                st = real_lstat(path_self)
                return SimpleNamespace(st_mode=st.st_mode, st_uid=0, st_gid=0, st_size=st.st_size)

            calls = []

            def fake_run(*argv, check=True):
                calls.append(argv)
                return subprocess.CompletedProcess(argv, 0, '', '')

            with mock.patch.object(helper, 'ETC_SSH_SOCKET', etc_sock), \
                 mock.patch.object(helper, 'RUN_SSH_SOCKET', run_sock), \
                 mock.patch.object(helper, 'protected_parent'), \
                 mock.patch.object(Path, 'lstat', root_lstat), \
                 mock.patch.object(helper, 'sshd_processes', return_value=[]), \
                 mock.patch.object(helper, 'run', side_effect=fake_run):
                helper.stop_old_sshd()
            self.assertEqual(calls, [('/usr/bin/systemctl', 'stop', 'ssh.service', 'ssh.socket')])

            # Unmasked socket falls back to runtime mask + stop.
            etc_sock.unlink()
            calls.clear()
            with mock.patch.object(helper, 'ETC_SSH_SOCKET', etc_sock), \
                 mock.patch.object(helper, 'RUN_SSH_SOCKET', run_sock), \
                 mock.patch.object(helper, 'protected_parent'), \
                 mock.patch.object(Path, 'lstat', root_lstat), \
                 mock.patch.object(helper, 'sshd_processes', return_value=[]), \
                 mock.patch.object(helper, 'run', side_effect=fake_run):
                helper.stop_old_sshd()
            self.assertEqual(calls, [
                ('/usr/bin/systemctl', 'mask', '--runtime', '--now', 'ssh.socket'),
                ('/usr/bin/systemctl', 'stop', 'ssh.service'),
            ])

    def test_dockerfile_prestages_dropin_legacy_archive_and_masked_socket(self):
        dockerfile = (HERE / 'Dockerfile').read_text()
        self.assertIn('/var/lib/boat-ssh/legacy', dockerfile)
        self.assertIn('/etc/boat/ssh-legacy-archive', dockerfile)
        self.assertIn('DROPIN_PATH', dockerfile)
        self.assertIn('SYSTEMD_POLICY', dockerfile)
        self.assertIn('SSHD_CONFIG', dockerfile)
        self.assertIn('sshd_policy', dockerfile)
        self.assertRegex(dockerfile, r'systemctl mask[^\n]*\bssh\.socket\b')

    def test_dockerfile_baked_sshd_config_matches_rendered_policy_byte_for_byte(self):
        import importlib.machinery
        import re
        from types import SimpleNamespace
        dockerfile = (HERE / 'Dockerfile').read_text()
        snippets = re.findall(r"python3 -I -c '([^']*boat-ssh-authorize[^']*)'", dockerfile)
        self.assertEqual(len(snippets), 2, 'expected slim and full sshd_config staging snippets in Dockerfile')

        real_fstat = os.fstat
        for snippet, users in zip(snippets, [['dev'], ['dev', 'user']]):
            with self.subTest(users=users), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                dropin_path = root / 'etc' / 'systemd' / 'system' / 'ssh.service.d' / 'boat-authority.conf'
                sshd_cfg = root / 'etc' / 'ssh' / 'sshd_config'
                sshd_cfg.parent.mkdir(parents=True, exist_ok=True)
                helper_src = str(HERE / 'ssh-authorize.py')
                # Point the snippet's SourceFileLoader at our checkout ssh-authorize.py.
                patched_snippet = snippet.replace('/usr/local/libexec/boat-ssh-authorize', helper_src)

                def fake_fstat(fd):
                    st = real_fstat(fd)
                    return SimpleNamespace(st_mode=st.st_mode, st_uid=0, st_gid=0, st_size=st.st_size,
                                           st_ino=st.st_ino, st_dev=st.st_dev)

                orig_exec_module = importlib.machinery.SourceFileLoader.exec_module

                def patched_exec_module(loader_self, mod):
                    orig_exec_module(loader_self, mod)
                    mod.DROPIN_PATH = dropin_path
                    mod.SSHD_CONFIG = sshd_cfg
                    mod.protected_parent = lambda _p: None
                    mod.installed_users = lambda: list(users)

                with mock.patch.object(os, 'fstat', fake_fstat), \
                     mock.patch.object(importlib.machinery.SourceFileLoader, 'exec_module', patched_exec_module):
                    exec(patched_snippet, {})

                expected = helper.sshd_policy(users).encode()
                self.assertTrue(sshd_cfg.is_file())
                self.assertEqual(sshd_cfg.read_bytes(), expected)
                self.assertEqual(stat.S_IMODE(sshd_cfg.stat().st_mode), 0o644)
                if 'DROPIN_PATH' in snippet:
                    self.assertEqual(dropin_path.read_bytes(), helper.SYSTEMD_POLICY.encode())
                    self.assertEqual(stat.S_IMODE(dropin_path.stat().st_mode), 0o644)

    def test_install_and_verify_fast_path_single_systemctl_and_skips_redundant_sshd_T(self):
        from types import SimpleNamespace
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cfg_path = root / 'etc' / 'boat' / 'ssh-auth.json'
            archive_path = root / 'etc' / 'boat' / 'ssh-legacy-archive'
            sshd_cfg = root / 'etc' / 'ssh' / 'sshd_config'
            dropin_path = root / 'etc' / 'systemd' / 'system' / 'ssh.service.d' / 'boat-authority.conf'
            helper_path = root / 'usr' / 'local' / 'libexec' / 'boat-ssh-authorize'
            run_dir = root / 'run' / 'sshd'
            sshd_bin = root / 'usr' / 'sbin' / 'sshd'
            host_ed = root / 'etc' / 'ssh' / 'ssh_host_ed25519_key'
            host_ecdsa = root / 'etc' / 'ssh' / 'ssh_host_ecdsa_key'
            home_dev = root / 'home' / 'dev'
            for d in [cfg_path.parent, sshd_cfg.parent, dropin_path.parent, helper_path.parent,
                      run_dir, sshd_bin.parent, home_dev / '.ssh']:
                d.mkdir(parents=True, exist_ok=True)
            sshd_cfg.write_bytes(helper.sshd_policy(CONFIG['users']).encode())
            sshd_cfg.chmod(0o644)
            dropin_path.write_bytes(helper.SYSTEMD_POLICY.encode())
            dropin_path.chmod(0o644)
            helper_path.write_bytes(b'#!/usr/bin/python3 -I\n')
            helper_path.chmod(0o555)
            sshd_bin.write_bytes(b'')
            sshd_bin.chmod(0o755)
            host_ed.write_bytes(b'ed25519')
            host_ecdsa.write_bytes(b'ecdsa')
            for p in (sshd_cfg, dropin_path, host_ed, host_ecdsa):
                os.utime(p, (1000.0, 1000.0))

            good_show = (
                'Id=ssh.service\n'
                'ActiveState=active\n'
                'KillMode=control-group\n'
                'ExecStart={ path=/usr/sbin/sshd ; argv[]=/usr/sbin/sshd -D -f /etc/ssh/sshd_config ; ignore_errors=no ; }\n'
                'MainPID=4242\n'
                '\n'
                'Id=ssh.socket\n'
                'ActiveState=inactive\n'
                'KillMode=control-group\n'
                'MainPID=0\n'
            )
            sshd_t_out = '\n'.join([
                'authorizedkeysfile .ssh/authorized_keys.tenant',
                'authorizedkeyscommand /usr/bin/python3 -I /usr/local/libexec/boat-ssh-authorize authorize %u %t %k',
                'authorizedkeyscommanduser boat-ssh',
                'trustedusercakeys none',
                'authorizedprincipalsfile none',
                'authorizedprincipalscommand none',
                'authenticationmethods publickey',
                'passwordauthentication no',
                'kbdinteractiveauthentication no',
                'permitrootlogin no',
                'hostbasedauthentication no',
                'pubkeyauthentication yes',
                'strictmodes yes',
                'permituserenvironment no',
                'allowusers dev',
                'allowusers user',
            ]) + '\n'

            commands = []
            writes = []

            def fake_run(*argv, check=True):
                commands.append(argv)
                if 'show' in argv:
                    return subprocess.CompletedProcess(argv, 0, good_show, '')
                if '-T' in argv:
                    return subprocess.CompletedProcess(argv, 0, sshd_t_out, '')
                return subprocess.CompletedProcess(argv, 0, '', '')

            def fake_atomic(path, data, mode, gid=0):
                path = Path(path)
                writes.append(path)
                path.write_bytes(data)
                path.chmod(mode)

            real_fstat = os.fstat
            real_lstat = Path.lstat

            def root_fstat(fd):
                st = real_fstat(fd)
                is_cfg = cfg_path.exists() and st.st_ino == real_lstat(cfg_path).st_ino
                return SimpleNamespace(st_mode=st.st_mode, st_uid=0, st_gid=42 if is_cfg else 0,
                                       st_size=st.st_size, st_mtime=st.st_mtime)

            def root_lstat(path_self):
                st = real_lstat(path_self)
                return SimpleNamespace(st_mode=st.st_mode, st_uid=0, st_gid=0,
                                       st_size=st.st_size, st_mtime=st.st_mtime)

            def fake_pwnam(name):
                if name == 'boat-ssh':
                    return SimpleNamespace(pw_uid=999, pw_gid=42, pw_shell='/usr/sbin/nologin', pw_dir='/nonexistent')
                return SimpleNamespace(pw_uid=1000, pw_gid=1000, pw_shell='/bin/bash', pw_dir=str(home_dev))

            exe_target = [str(sshd_bin)]

            with contextlib.ExitStack() as stack:
                for p in [
                    mock.patch.object(helper, 'CONFIG_PATH', cfg_path),
                    mock.patch.object(helper, 'ARCHIVE', archive_path),
                    mock.patch.object(helper, 'SSHD_CONFIG', sshd_cfg),
                    mock.patch.object(helper, 'DROPIN_PATH', dropin_path),
                    mock.patch.object(helper, 'HELPER_PATH', helper_path),
                    mock.patch.object(helper, 'SSHD_RUN_DIR', run_dir),
                    mock.patch.object(helper, 'SSHD_BIN', sshd_bin),
                    mock.patch.object(helper, 'HOST_KEYS', (host_ed, host_ecdsa)),
                    mock.patch.object(helper.os, 'geteuid', return_value=0),
                    mock.patch.object(helper.os, 'fstat', side_effect=root_fstat),
                    mock.patch.object(Path, 'lstat', root_lstat),
                    mock.patch.object(helper, 'protected_parent'),
                    mock.patch.object(helper, 'stop_old_sshd', side_effect=AssertionError('unexpected stop')),
                    mock.patch.object(helper.pwd, 'getpwnam', side_effect=fake_pwnam),
                    mock.patch.object(helper.grp, 'getgrnam', return_value=SimpleNamespace(gr_gid=42)),
                    mock.patch.object(helper, 'atomic_file', side_effect=fake_atomic),
                    mock.patch.object(helper, 'run', side_effect=fake_run),
                    mock.patch.object(helper, 'process_start_time', return_value=2000.0),
                    mock.patch.object(helper.os, 'readlink', side_effect=lambda _p: exe_target[0]),
                ]:
                    stack.enter_context(p)

                # 1. Unchanged baked config + running sshd: single systemctl show, zero sshd -T/-t, zero restart.
                helper.install_and_verify(CONFIG)
                self.assertEqual(writes, [cfg_path])
                self.assertEqual(len(commands), 1)
                self.assertEqual(commands[0][:4], ('/usr/bin/systemctl', 'show', 'ssh.service', 'ssh.socket'))
                self.assertIn('--property=MainPID', commands[0])
                self.assertFalse(any('-T' in c or '-t' in c for c in commands))

                # 2. Standalone verify() ALWAYS runs full sshd -T for every installed user.
                commands.clear()
                helper.verify(CONFIG)
                sshd_t_calls = [c for c in commands if '-T' in c]
                self.assertEqual(len(sshd_t_calls), 2)

                # 3. If /proc/<pid>/exe does not match SSHD_BIN mid-flight, verify_policy falls back to full sshd -T.
                commands.clear()
                exe_target[0] = str(sshd_bin) + ' (deleted)'
                helper.install_and_verify(CONFIG)
                self.assertEqual(len([c for c in commands if '-T' in c]), 2)
                exe_target[0] = str(sshd_bin)

                # 4. If host keys were regenerated after sshd started (e.g. defer_ssh_restart), sshd_unchanged is False.
                os.utime(host_ed, (3000.0, 3000.0))
                self.assertFalse(helper.sshd_unchanged(CONFIG))

    def test_rotate_token_happy_path_and_fail_closed_boundaries(self):
        from types import SimpleNamespace
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cfg_path = root / 'etc' / 'boat' / 'ssh-auth.json'
            archive_path = root / 'etc' / 'boat' / 'ssh-legacy-archive'
            sshd_cfg = root / 'etc' / 'ssh' / 'sshd_config'
            dropin_path = root / 'etc' / 'systemd' / 'system' / 'ssh.service.d' / 'boat-authority.conf'
            helper_path = root / 'usr' / 'local' / 'libexec' / 'boat-ssh-authorize'
            sshd_bin = root / 'usr' / 'sbin' / 'sshd'
            home_dev = root / 'home' / 'dev'
            for d in [cfg_path.parent, sshd_cfg.parent, dropin_path.parent, helper_path.parent,
                      sshd_bin.parent, home_dev / '.ssh']:
                d.mkdir(parents=True, exist_ok=True)
            archive_path.mkdir(mode=0o700, parents=True, exist_ok=True)
            archive_path.chmod(0o700)

            old_config = dict(CONFIG, token='ssh-auth-v1.old', generation=1)
            new_config = dict(CONFIG, token='ssh-auth-v1.rotated', generation=2)

            def reset_valid_files():
                if archive_path.is_symlink() or not archive_path.exists():
                    if archive_path.is_symlink():
                        archive_path.unlink()
                    archive_path.mkdir(mode=0o700, parents=True, exist_ok=True)
                archive_path.chmod(0o700)
                for p in (cfg_path, sshd_cfg, dropin_path, helper_path):
                    if p.is_symlink() or p.exists():
                        p.unlink()
                cfg_path.write_bytes(helper.canonical(old_config))
                cfg_path.chmod(0o640)
                sshd_cfg.write_bytes(helper.sshd_policy(CONFIG['users']).encode())
                sshd_cfg.chmod(0o644)
                dropin_path.write_bytes(helper.SYSTEMD_POLICY.encode())
                dropin_path.chmod(0o644)
                helper_path.write_bytes(b'#!/usr/bin/python3 -I\n')
                helper_path.chmod(0o555)
                sshd_bin.write_bytes(b'')
                sshd_bin.chmod(0o755)

            reset_valid_files()

            good_service_out = (
                'Id=ssh.service\n'
                'ActiveState=active\n'
                'KillMode=control-group\n'
                'ExecStart={ path=/usr/sbin/sshd ; argv[]=/usr/sbin/sshd -D -f /etc/ssh/sshd_config ; ignore_errors=no ; }\n'
                '\n'
                'Id=ssh.socket\n'
                'ActiveState=inactive\n'
                'KillMode=control-group\n'
            )
            service_state = {'stdout': good_service_out, 'rc': 0}
            commands = []
            writes = []

            def fake_run(*argv, check=True):
                commands.append(argv)
                if service_state['rc'] != 0 and check:
                    raise subprocess.CalledProcessError(service_state['rc'], argv)
                return subprocess.CompletedProcess(argv, service_state['rc'], service_state['stdout'], '')

            def fake_atomic(path, data, mode, gid=0):
                path = Path(path)
                writes.append((path, mode, gid))
                path.write_bytes(data)
                path.chmod(mode)

            real_fstat = os.fstat
            real_lstat = Path.lstat
            fstat_override = {}

            def root_fstat(fd):
                st = real_fstat(fd)
                is_cfg = cfg_path.exists() and not cfg_path.is_symlink() and st.st_ino == real_lstat(cfg_path).st_ino
                gid = 42 if is_cfg else 0
                base = dict(st_mode=st.st_mode, st_uid=0, st_gid=gid, st_size=st.st_size)
                if is_cfg and fstat_override:
                    base.update(fstat_override)
                return SimpleNamespace(**base)

            def root_lstat(path_self):
                st = real_lstat(path_self)
                return SimpleNamespace(st_mode=st.st_mode, st_uid=0, st_gid=0, st_size=st.st_size)

            account_state = {'boat_ssh_uid': 999, 'boat_ssh_shell': '/usr/sbin/nologin', 'missing_boat_ssh': False}

            def fake_pwnam(name):
                if name == 'boat-ssh':
                    if account_state['missing_boat_ssh']:
                        raise KeyError('boat-ssh')
                    return SimpleNamespace(pw_uid=account_state['boat_ssh_uid'], pw_gid=42,
                                           pw_shell=account_state['boat_ssh_shell'], pw_dir='/nonexistent')
                if name in ('dev', 'user'):
                    return SimpleNamespace(pw_uid=1000, pw_gid=1000, pw_shell='/bin/bash', pw_dir=str(home_dev))
                raise KeyError(name)

            with contextlib.ExitStack() as stack:
                for p in [
                    mock.patch.object(helper, 'CONFIG_PATH', cfg_path),
                    mock.patch.object(helper, 'ARCHIVE', archive_path),
                    mock.patch.object(helper, 'SSHD_CONFIG', sshd_cfg),
                    mock.patch.object(helper, 'DROPIN_PATH', dropin_path),
                    mock.patch.object(helper, 'HELPER_PATH', helper_path),
                    mock.patch.object(helper, 'SSHD_BIN', sshd_bin),
                    mock.patch.object(helper.os, 'geteuid', return_value=0),
                    mock.patch.object(helper.os, 'fstat', side_effect=root_fstat),
                    mock.patch.object(Path, 'lstat', root_lstat),
                    mock.patch.object(helper, 'protected_parent'),
                    mock.patch.object(helper, 'stop_old_sshd', side_effect=AssertionError('unexpected stop_old_sshd')),
                    mock.patch.object(helper, 'terminate_sshd', side_effect=AssertionError('unexpected terminate_sshd')),
                    mock.patch.object(helper, 'sshd_processes', side_effect=AssertionError('unexpected sshd_processes')),
                    mock.patch.object(helper.pwd, 'getpwnam', side_effect=fake_pwnam),
                    mock.patch.object(helper.grp, 'getgrnam', return_value=SimpleNamespace(gr_gid=42)),
                    mock.patch.object(helper, 'atomic_file', side_effect=fake_atomic),
                    mock.patch.object(helper, 'run', side_effect=fake_run),
                ]:
                    stack.enter_context(p)

                # 1. Happy path via main(['rotate-token']): atomic 0640 update, zero sshd restarts/kills/sshd -T.
                out = io.StringIO()
                stdin_buf = io.BytesIO(helper.canonical(new_config))
                with mock.patch.object(helper.sys, 'stdin', SimpleNamespace(buffer=stdin_buf)), \
                     contextlib.redirect_stdout(out):
                    rc = helper.main(['rotate-token'])
                self.assertEqual(rc, 0)
                self.assertEqual(json.loads(out.getvalue()), {
                    'version': 1,
                    'config_digest': helper.config_digest(new_config),
                    'boot_epoch': new_config['boot_epoch'],
                    'readiness': True,
                })
                self.assertEqual(writes, [(cfg_path, 0o640, 42)])
                self.assertEqual(cfg_path.read_bytes(), helper.canonical(new_config))
                self.assertEqual(helper.read_config(cfg_path), new_config)
                self.assertEqual(len(commands), 1)
                self.assertEqual(commands[0][:4], ('/usr/bin/systemctl', 'show', 'ssh.service', 'ssh.socket'))
                self.assertFalse(any('stop' in c or 'start' in c or 'restart' in c for c in commands))
                self.assertFalse(any(str(sshd_bin) in c for c in commands))

                # 2. Stdin framing/validation failures return 1 with empty stdout and do not touch CONFIG_PATH.
                for bad_stdin in [b'x' * (helper.CONFIG_LIMIT + 1),
                                  b'{"version":1,"version":1}',
                                  json.dumps(dict(new_config, token='bad\n')).encode()]:
                    reset_valid_files()
                    writes.clear()
                    bad_out = io.StringIO()
                    with mock.patch.object(helper.sys, 'stdin', SimpleNamespace(buffer=io.BytesIO(bad_stdin))), \
                         contextlib.redirect_stdout(bad_out):
                        self.assertEqual(helper.main(['rotate-token']), 1)
                    self.assertEqual(bad_out.getvalue(), '')
                    self.assertEqual(writes, [])
                    self.assertEqual(cfg_path.read_bytes(), helper.canonical(old_config))

                # 3. Fail-closed pre-write checks when policy, accounts, permissions, or ssh.service diverge.
                def assert_rejected(cfg=new_config):
                    writes.clear()
                    with self.assertRaises(ValueError):
                        helper.rotate_token(cfg)
                    self.assertEqual(writes, [])
                    self.assertEqual(cfg_path.read_bytes(), helper.canonical(old_config))

                # Divergent users list vs installed AllowUsers in SSHD_CONFIG
                reset_valid_files()
                assert_rejected(dict(new_config, users=['dev']))

                # Divergent SSHD_CONFIG contents, mode, or symlink
                reset_valid_files()
                sshd_cfg.write_bytes(helper.sshd_policy(CONFIG['users']).encode() + b'\nPermitRootLogin yes\n')
                assert_rejected()

                reset_valid_files()
                sshd_cfg.chmod(0o600)
                assert_rejected()

                reset_valid_files()
                other_sshd = root / 'other-sshd'
                other_sshd.write_bytes(helper.sshd_policy(CONFIG['users']).encode())
                sshd_cfg.unlink()
                sshd_cfg.symlink_to(other_sshd)
                assert_rejected()

                # Divergent DROPIN_PATH contents, mode, missing, or symlink
                reset_valid_files()
                dropin_path.write_bytes(b'[Service]\nExecStart=/bin/false\n')
                assert_rejected()

                reset_valid_files()
                dropin_path.chmod(0o600)
                assert_rejected()

                reset_valid_files()
                dropin_path.unlink()
                assert_rejected()

                reset_valid_files()
                other_dropin = root / 'other-dropin'
                other_dropin.write_bytes(helper.SYSTEMD_POLICY.encode())
                dropin_path.unlink()
                dropin_path.symlink_to(other_dropin)
                assert_rejected()

                # Divergent HELPER_PATH mode or symlink
                reset_valid_files()
                helper_path.chmod(0o755)
                assert_rejected()

                reset_valid_files()
                helper_path.unlink()
                helper_path.symlink_to(other_dropin)
                assert_rejected()

                # Unsafe ARCHIVE mode or symlink, or unsafe protected_parent
                reset_valid_files()
                archive_path.chmod(0o755)
                assert_rejected()

                reset_valid_files()
                archive_path.rmdir()
                archive_path.symlink_to(other_dropin)
                assert_rejected()

                reset_valid_files()
                with mock.patch.object(helper, 'protected_parent', side_effect=ValueError('untrusted parent')):
                    assert_rejected()

                # Missing or misconfigured boat-ssh account
                reset_valid_files()
                account_state['missing_boat_ssh'] = True
                try:
                    assert_rejected()
                finally:
                    account_state['missing_boat_ssh'] = False

                account_state['boat_ssh_uid'] = 0
                try:
                    assert_rejected()
                finally:
                    account_state['boat_ssh_uid'] = 999

                # Divergent ssh.service / ssh.socket states
                reset_valid_files()
                for bad_svc in [
                    good_service_out.replace('Id=ssh.service\nActiveState=active', 'Id=ssh.service\nActiveState=inactive'),
                    good_service_out.replace('Id=ssh.socket\nActiveState=inactive', 'Id=ssh.socket\nActiveState=active'),
                    good_service_out.replace('KillMode=control-group', 'KillMode=process', 1),
                    good_service_out.replace('/etc/ssh/sshd_config', '/etc/ssh/sshd_config -o PermitRootLogin=yes'),
                ]:
                    service_state['stdout'] = bad_svc
                    assert_rejected()
                service_state['stdout'] = good_service_out
                service_state['rc'] = 3
                try:
                    assert_rejected()
                finally:
                    service_state['rc'] = 0

                # Post-write verify_config_file catches wrong permissions/gid on CONFIG_PATH
                reset_valid_files()
                fstat_override.update(st_gid=0)
                try:
                    with self.assertRaises(ValueError):
                        helper.rotate_token(new_config)
                finally:
                    fstat_override.clear()


class HostKeyRepairTests(unittest.TestCase):
    def test_repairs_symlink_without_touching_target_and_fsyncs_pair_and_directory(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            target = root / 'target'
            target.write_bytes(b'leave intact')
            key = root / 'ssh_host_ed25519_key'
            key.symlink_to(target)
            public = Path(str(key) + '.pub')
            public.symlink_to(target)
            synced = []
            real_fsync = os.fsync

            def fsync(fd):
                info = os.fstat(fd)
                synced.append((info.st_dev, info.st_ino))
                real_fsync(fd)

            def keygen(*args, **kwargs):
                self.assertEqual(args, ('/usr/bin/ssh-keygen', '-q', '-t',
                                       'ed25519', '-N', '', '-f', str(key)))
                self.assertFalse(key.is_symlink())
                self.assertFalse(public.is_symlink())
                key.write_bytes(b'private stub')
                public.write_bytes(b'public stub')

            with mock.patch.object(helper, 'HOST_KEYS', (key,)), \
                 mock.patch.object(helper, 'protected_parent') as parent, \
                 mock.patch.object(helper, 'run', side_effect=keygen) as command, \
                 mock.patch.object(helper.os, 'fsync', side_effect=fsync):
                helper.ensure_host_keys()
                helper.ensure_host_keys()  # Nonempty stubs stay compatible and need no repair.
            parent.assert_has_calls([mock.call(key), mock.call(key)])
            command.assert_called_once()
            self.assertEqual(target.read_bytes(), b'leave intact')
            self.assertEqual(synced, [(p.stat().st_dev, p.stat().st_ino)
                                      for p in (key, public, root)])


class LiveSshdLoginTests(unittest.TestCase):
    def test_real_sshd_tenant_enrollment_legacy_and_ca_denial(self):
        if not Path('/usr/sbin/sshd').exists():
            self.skipTest('local sshd unavailable')
        import pwd
        with tempfile.TemporaryDirectory(prefix='boat-live-sshd-') as tmp:
            root = Path(tmp)
            user = pwd.getpwuid(os.getuid()).pw_name
            for name in ['host', 'tenant-key', 'legacy-key', 'cert-key', 'ca']:
                subprocess.run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(root / name)],
                               check=True, capture_output=True, timeout=10)
            subprocess.run(['ssh-keygen', '-q', '-s', str(root / 'ca'), '-I', 'test', '-n', user,
                            str(root / 'cert-key.pub')], check=True, capture_output=True, timeout=10)
            (root / 'authorized_keys').write_bytes((root / 'legacy-key.pub').read_bytes())
            (root / 'authorized_keys2').write_bytes((root / 'legacy-key.pub').read_bytes())
            with socket.socket() as sock:
                sock.bind(('127.0.0.1', 0))
                port = sock.getsockname()[1]
            policy = helper.sshd_policy(['dev'])
            # A local nonroot harness cannot create guest accounts/system paths.
            # Replace host paths and local account only. The actual auth directives remain.
            policy = policy.replace('AllowUsers dev', 'AllowUsers ' + user)
            policy = policy.replace('UsePAM yes', 'UsePAM no')
            policy = policy.replace('StrictModes yes', 'StrictModes no')
            policy = policy.replace('AuthorizedKeysCommandUser boat-ssh', 'AuthorizedKeysCommandUser ' + user)
            policy = policy.replace('AuthorizedKeysFile .ssh/authorized_keys.tenant',
                                    'AuthorizedKeysFile ' + str(root / 'authorized_keys.tenant'))
            # No online config in this harness; /usr/bin/false represents a denied lookup.
            policy = policy.replace('AuthorizedKeysCommand /usr/bin/python3 -I /usr/local/libexec/boat-ssh-authorize authorize %u %t %k',
                                    'AuthorizedKeysCommand /usr/bin/false')
            policy = policy.replace('HostKey /etc/ssh/ssh_host_ed25519_key', 'HostKey ' + str(root / 'host'))
            policy = policy.replace('HostKey /etc/ssh/ssh_host_ecdsa_key\n', '')
            policy += f'Port {port}\nListenAddress 127.0.0.1\nPidFile {root}/pid\n'
            (root / 'config').write_text(policy)
            options = subprocess.run(['/usr/sbin/sshd', '-T', '-f', str(root / 'config')],
                                     capture_output=True, text=True, check=True, timeout=10).stdout
            if 'persourcepenalties ' in options:
                # OpenSSH10's accumulated failure penalties obscure independent attempts.
                (root / 'config').write_text(policy + 'PerSourcePenalties no\n')
            with (root / 'log').open('w') as log:
                proc = subprocess.Popen(['/usr/sbin/sshd', '-D', '-e', '-f', str(root / 'config')],
                                        stdout=log, stderr=log)
                try:
                    deadline = time.monotonic() + 3
                    while True:
                        if proc.poll() is not None:
                            self.fail('local sshd failed: ' + (root / 'log').read_text())
                        try:
                            with socket.create_connection(('127.0.0.1', port), timeout=.1):
                                break
                        except OSError:
                            if time.monotonic() > deadline:
                                self.fail('local sshd startup timeout')
                            time.sleep(.02)
                    def login(key):
                        return subprocess.run(['ssh', '-F', '/dev/null', '-p', str(port), '-i', str(root / key),
                            '-o', 'BatchMode=yes', '-o', 'IdentitiesOnly=yes', '-o', 'IdentityAgent=none',
                            '-o', 'StrictHostKeyChecking=no', '-o', 'UserKnownHostsFile=/dev/null',
                            '-o', 'ConnectTimeout=2', user + '@127.0.0.1', 'true'],
                            capture_output=True, timeout=5).returncode
                    self.assertNotEqual(login('legacy-key'), 0)
                    self.assertNotEqual(login('tenant-key'), 0)
                    self.assertNotEqual(login('cert-key'), 0)
                    (root / 'authorized_keys.tenant').write_bytes((root / 'tenant-key.pub').read_bytes())
                    self.assertEqual(login('tenant-key'), 0, (root / 'log').read_text())
                    self.assertNotEqual(login('legacy-key'), 0)
                    self.assertNotEqual(login('cert-key'), 0)
                    (root / 'authorized_keys.tenant').unlink()
                    self.assertNotEqual(login('tenant-key'), 0)
                finally:
                    proc.terminate()
                    try:
                        proc.wait(timeout=3)
                    except subprocess.TimeoutExpired:
                        proc.kill()
                        proc.wait(timeout=3)


class PidfdTeardownTests(unittest.TestCase):
    def test_pidfd_checks_executable_after_open(self):
        events=[]
        def opened(pid,flags=0):
            events.append('open');return 55
        def executable(path):
            events.append('exe');return '/usr/sbin/sshd'
        with mock.patch.object(helper.os,'pidfd_open',opened,create=True), mock.patch.object(helper.os,'readlink',executable), mock.patch.object(helper.signal,'pidfd_send_signal',create=True) as send, mock.patch.object(helper.os,'close') as close, mock.patch.object(helper.os,'kill') as numeric:
            helper.terminate_sshd(123)
        self.assertEqual(events,['open','exe'])
        send.assert_called_once_with(55,signal.SIGKILL,None,0)
        close.assert_called_once_with(55)
        numeric.assert_not_called()

    def test_reused_pid_is_not_signalled(self):
        with mock.patch.object(helper.os,'pidfd_open',return_value=55,create=True), mock.patch.object(helper.os,'readlink',return_value='/usr/bin/innocent'), mock.patch.object(helper.signal,'pidfd_send_signal',create=True) as send, mock.patch.object(helper.os,'close') as close:
            helper.terminate_sshd(123)
        send.assert_not_called();close.assert_called_once_with(55)

    def test_unavailable_pidfd_fails_without_numeric_fallback(self):
        with mock.patch.object(helper.os,'pidfd_open',side_effect=OSError('unsupported'),create=True), mock.patch.object(helper.signal,'pidfd_send_signal',create=True), mock.patch.object(helper.os,'kill') as numeric, self.assertRaises(OSError):
            helper.terminate_sshd(123)
        numeric.assert_not_called()


class SecurityKeyCompatibilityTests(unittest.TestCase):
    def wire(self, *parts):
        return base64.b64encode(b''.join(struct.pack('>I',len(part))+part for part in parts)).decode()

    def test_security_key_types_and_exact_response(self):
        types=[(b'sk-ssh-ed25519@openssh.com',[b'k'*32,b'ssh:test']),
               (b'sk-ecdsa-sha2-nistp256@openssh.com',[b'nistp256',b'\x04'+b'k'*64,b'ssh:test'])]
        for typ,parts in types:
            blob=self.wire(typ,*parts)
            with self.subTest(typ=typ):
                helper.validate_key(typ.decode(),blob)
                line=typ.decode()+' '+blob
                self.assertEqual(helper.authorized_response(json.dumps({'authorized_key':line}).encode(),typ.decode(),blob),line)
                for badparts in [parts[:-1],parts+[b'trailing'],parts[:-1]+[b'']]:
                    with self.assertRaises(ValueError):
                        helper.validate_key(typ.decode(),self.wire(typ,*badparts))
        for typ,parts in [(b'sk-ssh-ed25519@openssh.com',[b'short',b'ssh:test']),
                          (b'sk-ecdsa-sha2-nistp256@openssh.com',[b'nistp384',b'\x04'+b'k'*64,b'ssh:test']),
                          (b'sk-ecdsa-sha2-nistp256@openssh.com',[b'nistp256',b'bad',b'ssh:test'])]:
            with self.subTest(parts=parts), self.assertRaises(ValueError):
                helper.validate_key(typ.decode(),self.wire(typ,*parts))


if __name__ == '__main__':
    unittest.main()
