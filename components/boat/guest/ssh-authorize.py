#!/usr/bin/python3 -I
"""Root-owned online SSH authority. No tenant environment, cache or fallback.

Install/verify/install-and-verify/rotate-token: root, config JSON on stdin. Authorize: USER TYPE BLOB.
The caller must quarantine ingress throughout install and until attestation.
"""
import base64
import hashlib
import http.client
import json
import os
from pathlib import Path
import pwd
import grp
import re
import signal
import socket
import ssl
import stat
import struct
import subprocess
import sys
import tempfile
import time
import urllib.parse

CONFIG_PATH = Path('/etc/boat/ssh-auth.json')
HELPER_PATH = Path('/usr/local/libexec/boat-ssh-authorize')
SSHD_CONFIG = Path('/etc/ssh/sshd_config')
DROPIN_PATH = Path('/etc/systemd/system/ssh.service.d/boat-authority.conf')
ETC_SSH_SOCKET = Path('/etc/systemd/system/ssh.socket')
RUN_SSH_SOCKET = Path('/run/systemd/system/ssh.socket')
SSHD_RUN_DIR = Path('/run/sshd')
SSHD_BIN = Path('/usr/sbin/sshd')
HOST_KEYS = (Path('/etc/ssh/ssh_host_ed25519_key'), Path('/etc/ssh/ssh_host_ecdsa_key'))
ARCHIVE = Path('/etc/boat/ssh-legacy-archive')
CA_DIR = Path('/etc/ssl/certs')
CA_FILE = Path('/etc/ssl/certs/ca-certificates.crt')
LIMIT = 65536
CONFIG_LIMIT = 32768
HELPER_LIMIT = 1048576
ENV = {'PATH': '/usr/sbin:/usr/bin:/sbin:/bin', 'LANG': 'C'}
SYSTEMD_POLICY = '''[Service]
ExecStart=
ExecStart=/usr/sbin/sshd -D -f /etc/ssh/sshd_config
KillMode=control-group
'''


def strict_json(data):
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise ValueError('duplicate JSON key')
            result[key] = value
        return result
    return json.loads(data, object_pairs_hook=pairs,
                      parse_constant=lambda _: (_ for _ in ()).throw(ValueError('JSON constant')))


def canonical(config):
    return json.dumps(config, sort_keys=True, separators=(',', ':'), ensure_ascii=True).encode('ascii')


def config_digest(config):
    return hashlib.sha256(canonical(config)).hexdigest()


def validate_config(config):
    fields = {'version', 'url', 'token', 'vm_id', 'team_id', 'generation', 'boot_epoch', 'users'}
    if not isinstance(config, dict) or set(config) != fields:
        raise ValueError('config fields')
    if type(config['version']) is not int or config['version'] != 1:
        raise ValueError('version')
    if type(config['generation']) is not int or not 0 <= config['generation'] <= 2**53 - 1:
        raise ValueError('generation')
    for field in ('vm_id', 'team_id', 'boot_epoch'):
        if not isinstance(config[field], str) or not re.fullmatch(r'[A-Za-z0-9_-]{1,256}', config[field]):
            raise ValueError('identity')
    if not isinstance(config['token'], str) or not re.fullmatch(r'[A-Za-z0-9_.=-]{1,8192}', config['token']):
        raise ValueError('token')
    users = config['users']
    if not isinstance(users, list) or not users or len(users) != len(set(users)) or any(u not in ('dev', 'user') for u in users):
        raise ValueError('users')
    url = config['url']
    if not isinstance(url, str) or not url.isascii() or any(ord(c) <= 32 or ord(c) == 127 for c in url):
        raise ValueError('URL')
    parsed = urllib.parse.urlsplit(url)
    if (parsed.scheme != 'https' or not parsed.hostname or parsed.username is not None or
            parsed.password is not None or parsed.query or parsed.fragment or
            parsed.path != '/v1/vms/' + config['vm_id'] + '/ssh/authorize'):
        raise ValueError('HTTPS endpoint')
    if parsed.port is not None and not 1 <= parsed.port <= 65535:
        raise ValueError('port')
    if len(canonical(config)) > CONFIG_LIMIT:
        raise ValueError('config size')
    return config


def protected_parent(path):
    for parent in Path(path).absolute().parents:
        info = parent.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022:
            raise ValueError('untrusted parent')


def read_config(path=None):
    path = CONFIG_PATH if path is None else path
    protected_parent(path)
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, 'rb') as stream:
        info = os.fstat(stream.fileno())
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or
                stat.S_IMODE(info.st_mode) != 0o640 or info.st_gid != grp.getgrnam('boat-ssh').gr_gid):
            raise ValueError('untrusted config')
        data = stream.read(CONFIG_LIMIT + 1)
    if len(data) > CONFIG_LIMIT:
        raise ValueError('config size')
    return validate_config(strict_json(data))


def validate_key(key_type, key_blob):
    if key_type not in ('ssh-ed25519', 'ssh-rsa', 'ecdsa-sha2-nistp256', 'ecdsa-sha2-nistp384', 'ecdsa-sha2-nistp521',
                        'sk-ssh-ed25519@openssh.com', 'sk-ecdsa-sha2-nistp256@openssh.com'):
        raise ValueError('key type')
    if not isinstance(key_blob, str) or len(key_blob) > 24000 or not re.fullmatch(r'[A-Za-z0-9+/]+={0,2}', key_blob):
        raise ValueError('key encoding')
    try:
        raw = base64.b64decode(key_blob, validate=True)
    except Exception as error:
        raise ValueError('key encoding') from error
    if base64.b64encode(raw).decode('ascii') != key_blob or len(raw) < 4:
        raise ValueError('key encoding')
    size = struct.unpack('>I', raw[:4])[0]
    if size > len(raw) - 4 or raw[4:4 + size] != key_type.encode('ascii'):
        raise ValueError('key type mismatch')
    if key_type == 'ssh-ed25519' and (len(raw) != 4 + size + 4 + 32 or raw[4 + size:8 + size] != struct.pack('>I', 32)):
        raise ValueError('ed25519 key')
    if key_type.startswith('sk-'):
        # OpenSSH PROTOCOL.u2f: public fields followed by application string.
        offset = 4 + size

        def take():
            nonlocal offset
            if len(raw) - offset < 4:
                raise ValueError('security key field')
            length = struct.unpack('>I', raw[offset:offset + 4])[0]
            offset += 4
            if length > len(raw) - offset:
                raise ValueError('security key field')
            value = raw[offset:offset + length]
            offset += length
            return value

        if key_type == 'sk-ssh-ed25519@openssh.com':
            if len(take()) != 32:
                raise ValueError('security ed25519 key')
        else:
            curve, point = take(), take()
            if curve != b'nistp256' or len(point) != 65 or point[0] != 4:
                raise ValueError('security ecdsa key')
        application = take()
        if not application or b'\0' in application or offset != len(raw):
            raise ValueError('security key application/trailing bytes')


def authorized_response(data, key_type, key_blob):
    if len(data) > LIMIT:
        raise ValueError('response size')
    obj = strict_json(data)
    expected = key_type + ' ' + key_blob
    if not isinstance(obj, dict) or set(obj) != {'authorized_key'} or obj['authorized_key'] != expected:
        raise ValueError('response key')
    return expected


def _lookup_once(parsed, token, body, key_type, key_blob, *, capath=None, cafile=None):
    # Explicit verify locations prevent SSL_CERT_FILE / SSL_CERT_DIR environment use.
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    if capath is not None:
        context.load_verify_locations(capath=capath)
    else:
        context.load_verify_locations(cafile=cafile)
    conn = http.client.HTTPSConnection(parsed.hostname, parsed.port, timeout=0.5, context=context)
    try:
        conn.request('POST', parsed.path, body=body,
                     headers={'Authorization': 'Bearer ' + token,
                              'Content-Type': 'application/json', 'Accept': 'application/json',
                              'Cache-Control': 'no-store', 'Connection': 'close'})
        with conn.getresponse() as response:
            if response.status != 200:
                raise ValueError('authorization denied')
            if response.getheader('Content-Encoding', 'identity') != 'identity':
                raise ValueError('response encoding')
            lengths = response.headers.get_all('Content-Length', [])
            if (response.headers.defects or response.headers.get_all('Transfer-Encoding') is not None or
                    len(lengths) != 1 or not re.fullmatch(r'[0-9]+', lengths[0]) or int(lengths[0]) > LIMIT):
                raise ValueError('response framing')
            # Read through EOF: HTTPResponse otherwise hides bytes beyond Content-Length.
            response.length = None
            data = response.read(LIMIT + 1)
            if len(data) != int(lengths[0]):
                raise ValueError('response length')
            return authorized_response(data, key_type, key_blob)
    finally:
        conn.close()


def lookup(config, user, key_type, key_blob):
    validate_config(config)
    validate_key(key_type, key_blob)
    if user not in config['users']:
        raise ValueError('user')
    parsed = urllib.parse.urlsplit(config['url'])
    body = canonical(dict(user=user, key_type=key_type, key_blob=key_blob))
    try:
        use_capath = CA_DIR.is_dir()
    except OSError:
        use_capath = False
    if use_capath:
        try:
            return _lookup_once(parsed, config['token'], body, key_type, key_blob, capath=str(CA_DIR))
        except ssl.SSLCertVerificationError as cert_err:
            try:
                return _lookup_once(parsed, config['token'], body, key_type, key_blob, cafile=str(CA_FILE))
            except FileNotFoundError:
                raise cert_err
    return _lookup_once(parsed, config['token'], body, key_type, key_blob, cafile=str(CA_FILE))


def sshd_policy(users):
    return '''# Managed online authority; tenant keys require explicit enrollment.
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitEmptyPasswords no
PermitRootLogin no
PubkeyAuthentication yes
AuthenticationMethods publickey
HostbasedAuthentication no
GSSAPIAuthentication no
KerberosAuthentication no
StrictModes yes
UsePAM yes
AuthorizedKeysFile .ssh/authorized_keys.tenant
AuthorizedKeysCommand /usr/bin/python3 -I /usr/local/libexec/boat-ssh-authorize authorize %u %t %k
AuthorizedKeysCommandUser boat-ssh
TrustedUserCAKeys none
AuthorizedPrincipalsFile none
AuthorizedPrincipalsCommand none
PermitUserEnvironment no
AllowUsers ''' + ' '.join(users) + '''
HostKey /etc/ssh/ssh_host_ed25519_key
HostKey /etc/ssh/ssh_host_ecdsa_key
Subsystem sftp internal-sftp
'''


def run(*argv, check=True):
    return subprocess.run(argv, check=check, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                          env=ENV, timeout=15, text=True)


def atomic_file(path, data, mode, gid=0):
    path = Path(path)
    protected_parent(path)
    fd, temp = tempfile.mkstemp(prefix='.boat-ssh-', dir=path.parent)
    try:
        with os.fdopen(fd, 'wb') as stream:
            os.fchmod(stream.fileno(), mode)
            os.fchown(stream.fileno(), 0, gid)
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temp, path)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(temp):
            os.unlink(temp)


def archive_legacy(ssh_dirs, archive=None):
    archive = Path(ARCHIVE if archive is None else archive)
    archive.mkdir(mode=0o700, exist_ok=True)
    if archive.is_symlink() or stat.S_IMODE(archive.stat().st_mode) != 0o700:
        raise ValueError('archive permissions')
    for directory in ssh_dirs:
        for name in ('authorized_keys', 'authorized_keys2'):
            path = Path(directory) / name
            try:
                fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
            except FileNotFoundError:
                continue
            with os.fdopen(fd, 'rb') as stream:
                info = os.fstat(stream.fileno())
                if not stat.S_ISREG(info.st_mode) or info.st_size > 16 * 1024 * 1024:
                    raise ValueError('legacy file')
                data = stream.read(16 * 1024 * 1024 + 1)
            if len(data) > 16 * 1024 * 1024:
                raise ValueError('legacy size')
            dest = archive / (hashlib.sha256(str(path).encode() + b'\0' + data).hexdigest() + '.keys')
            fd = os.open(dest, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600) if not dest.exists() else None
            if fd is not None:
                with os.fdopen(fd, 'wb') as stream:
                    stream.write(data)
                    stream.flush()
                    os.fsync(stream.fileno())


def installed_users():
    users = []
    for name in ('dev', 'user'):
        try:
            account = pwd.getpwnam(name)
        except KeyError:
            continue
        if account.pw_uid != 1000:
            raise ValueError('guest user uid')
        users.append(name)
    if 'dev' not in users:
        raise ValueError('missing dev account')
    return users


def check_accounts(config):
    users = installed_users()
    if any(user not in users for user in config['users']):
        raise ValueError('missing configured account')
    try:
        account = pwd.getpwnam('boat-ssh')
        group = grp.getgrnam('boat-ssh')
    except KeyError as error:
        raise ValueError('helper account') from error
    if (account.pw_uid == 0 or account.pw_gid != group.gr_gid or
            account.pw_shell not in ('/usr/sbin/nologin', '/sbin/nologin') or account.pw_dir != '/nonexistent'):
        raise ValueError('helper account')
    return users, group.gr_gid


def exact_root_file(path, expected_bytes, mode=0o644):
    path = Path(path)
    try:
        protected_parent(path)
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except (FileNotFoundError, ValueError, OSError):
        return False
    with os.fdopen(fd, 'rb') as stream:
        info = os.fstat(stream.fileno())
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_gid != 0 or
                stat.S_IMODE(info.st_mode) != mode):
            return False
        data = stream.read(len(expected_bytes) + 1)
    return data == expected_bytes


def read_root_file(path, modes=(0o644,)):
    path = Path(path)
    protected_parent(path)
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, 'rb') as stream:
        info = os.fstat(stream.fileno())
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_gid != 0 or
                stat.S_IMODE(info.st_mode) not in modes):
            raise ValueError('untrusted policy file')
        data = stream.read(LIMIT + 1)
    if len(data) > LIMIT:
        raise ValueError('policy file size')
    return data


def ensure_sshd_run_dir(path=None):
    path = Path(SSHD_RUN_DIR if path is None else path)
    protected_parent(path)
    if not path.exists() and not path.is_symlink():
        path.mkdir(mode=0o755, parents=True, exist_ok=True)
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != 0 or info.st_gid != 0:
        raise ValueError('untrusted sshd runtime dir')
    if stat.S_IMODE(info.st_mode) != 0o755:
        os.chmod(path, 0o755)


def verify_files_and_config(config, check_syntax=True):
    users, _ = check_accounts(config)
    protected_parent(HELPER_PATH)
    info = HELPER_PATH.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_gid != 0 or stat.S_IMODE(info.st_mode) != 0o555:
        raise ValueError('helper permissions')
    if read_config() != config:
        raise ValueError('config mismatch')
    if read_root_file(SSHD_CONFIG, (0o644, 0o600)) != sshd_policy(config['users']).encode():
        raise ValueError('policy mismatch')
    if read_root_file(DROPIN_PATH, (0o644,)) != SYSTEMD_POLICY.encode():
        raise ValueError('dropin mismatch')
    if check_syntax and SSHD_BIN.exists():
        run(str(SSHD_BIN), '-t', '-f', str(SSHD_CONFIG))
    return users


def verify_policy(config):
    # `sshd -T` below validates config syntax + host keys and dumps effective policy in one pass.
    users = verify_files_and_config(config, check_syntax=False)
    if _fast_policy_verified(config):
        return users
    if not SSHD_BIN.exists():
        run(str(SSHD_BIN), '-t', '-f', str(SSHD_CONFIG))
    expected = {
        'authorizedkeysfile': '.ssh/authorized_keys.tenant',
        'authorizedkeyscommand': '/usr/bin/python3 -I /usr/local/libexec/boat-ssh-authorize authorize %u %t %k',
        'authorizedkeyscommanduser': 'boat-ssh', 'trustedusercakeys': 'none',
        'authorizedprincipalsfile': 'none', 'authorizedprincipalscommand': 'none',
        'authenticationmethods': 'publickey', 'passwordauthentication': 'no',
        'kbdinteractiveauthentication': 'no', 'permitrootlogin': 'no',
        'hostbasedauthentication': 'no', 'pubkeyauthentication': 'yes',
        'strictmodes': 'yes', 'permituserenvironment': 'no',
        'allowusers': ' '.join(config['users']),
    }
    # Verify every installed alias, including aliases deliberately denied by AllowUsers.
    for user in users:
        output = run(str(SSHD_BIN), '-T', '-f', str(SSHD_CONFIG), '-C',
                     'user=' + user + ',host=localhost,addr=127.0.0.1').stdout
        values = dict(line.split(' ', 1) for line in output.splitlines() if ' ' in line)
        values['allowusers'] = ' '.join(line.split(' ', 1)[1] for line in output.splitlines() if line.startswith('allowusers '))
        if any(values.get(key) != value for key, value in expected.items()):
            raise ValueError('effective sshd policy')
    return users


def parse_systemd_stanzas(text):
    stanzas = {}
    for block in text.strip().split('\n\n'):
        props = {}
        for line in block.splitlines():
            if '=' in line:
                key, val = line.split('=', 1)
                props[key] = val
        if 'Id' in props:
            stanzas[props['Id']] = props
    return stanzas


def verify_service():
    unit = run('/usr/bin/systemctl', 'show', 'ssh.service', 'ssh.socket',
               '--property=Id', '--property=ActiveState',
               '--property=KillMode', '--property=ExecStart',
               '--property=MainPID').stdout
    stanzas = parse_systemd_stanzas(unit)
    if ('ssh.service' in stanzas and 'ssh.socket' in stanzas and
            'ActiveState' in stanzas['ssh.service'] and 'ActiveState' in stanzas['ssh.socket']):
        if stanzas['ssh.socket']['ActiveState'] in ('active', 'activating', 'reloading'):
            raise ValueError('socket active')
        svc = stanzas['ssh.service']
        if svc.get('ActiveState') != 'active':
            raise ValueError('service inactive')
        commands = re.findall(r'argv\[\]=(.*?)\s*;', svc.get('ExecStart', ''))
        if svc.get('KillMode') != 'control-group' or commands != ['/usr/sbin/sshd -D -f /etc/ssh/sshd_config']:
            raise ValueError('effective service policy')
        raw_pid = svc.get('MainPID', '').strip()
        return int(raw_pid) if raw_pid.isdigit() else None
    if run('/usr/bin/systemctl', 'is-active', 'ssh.socket', check=False).returncode == 0:
        raise ValueError('socket active')
    run('/usr/bin/systemctl', 'is-active', '--quiet', 'ssh.service')
    commands = re.findall(r'argv\[\]=(.*?)\s*;', unit)
    if ('KillMode=control-group\n' not in unit or
            commands != ['/usr/sbin/sshd -D -f /etc/ssh/sshd_config']):
        raise ValueError('effective service policy')
    match = re.search(r'^MainPID=(\d+)$', unit, re.MULTILINE)
    return int(match.group(1)) if match else None


def is_masked_unit(path):
    path = Path(path)
    try:
        protected_parent(path)
        info = path.lstat()
        return stat.S_ISLNK(info.st_mode) and info.st_uid == 0 and os.readlink(path) == '/dev/null'
    except (OSError, ValueError):
        return False


def sshd_processes():
    pids = []
    for entry in Path('/proc').iterdir():
        if not entry.name.isdigit():
            continue
        try:
            # Match actual executable, including sshd-auth/session on newer OpenSSH.
            executable = os.readlink(entry / 'exe').removesuffix(' (deleted)')
            if executable in ('/usr/sbin/sshd', '/usr/lib/openssh/sshd-session', '/usr/lib/openssh/sshd-auth'):
                pids.append(int(entry.name))
        except FileNotFoundError:
            pass
    return pids


def terminate_sshd(pid):
    # A pidfd pins the signal target even when its numeric PID is recycled.
    # No numeric-PID fallback: unsupported kernels/Python keep ingress closed.
    opener = getattr(os, 'pidfd_open', None)
    sender = getattr(signal, 'pidfd_send_signal', None)
    if opener is None or sender is None:
        raise ValueError('pidfd support required')
    try:
        fd = opener(pid, 0)
    except ProcessLookupError:
        return
    try:
        try:
            executable = os.readlink('/proc/' + str(pid) + '/exe').removesuffix(' (deleted)')
            if executable in ('/usr/sbin/sshd', '/usr/lib/openssh/sshd-session', '/usr/lib/openssh/sshd-auth'):
                sender(fd, signal.SIGKILL, None, 0)
        except (FileNotFoundError, ProcessLookupError):
            pass
    finally:
        os.close(fd)


def stop_old_sshd():
    if is_masked_unit(ETC_SSH_SOCKET) or is_masked_unit(RUN_SSH_SOCKET):
        run('/usr/bin/systemctl', 'stop', 'ssh.service', 'ssh.socket')
    else:
        run('/usr/bin/systemctl', 'mask', '--runtime', '--now', 'ssh.socket')
        run('/usr/bin/systemctl', 'stop', 'ssh.service')
    # Kill preauth/session children surviving an old KillMode=process unit, and
    # standalone legacy listeners. Host ingress must already be quarantined.
    for pid in sshd_processes():
        terminate_sshd(pid)
    deadline = time.monotonic() + 2
    while sshd_processes():
        if time.monotonic() >= deadline:
            raise ValueError('old sshd survived')
        time.sleep(0.02)


def ensure_host_keys():
    """Repair keys lost to an unclean shutdown without following stale symlinks."""
    for key in HOST_KEYS:
        protected_parent(key)
        try:
            info = key.lstat()
            if stat.S_ISREG(info.st_mode) and info.st_size > 0:
                continue
        except FileNotFoundError:
            pass
        key.unlink(missing_ok=True)
        public = Path(str(key) + '.pub')
        public.unlink(missing_ok=True)
        key_type = key.name.removeprefix('ssh_host_').removesuffix('_key')
        run('/usr/bin/ssh-keygen', '-q', '-t', key_type, '-N', '', '-f', str(key))
        for path, mode in ((key, 0o600), (public, 0o644)):
            fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
            try:
                info = os.fstat(fd)
                if not stat.S_ISREG(info.st_mode) or info.st_size == 0:
                    raise ValueError('invalid generated host key')
                os.fchmod(fd, mode)
                os.fsync(fd)
            finally:
                os.close(fd)
        directory = os.open(key.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)


def install(config):
    global _UNCHANGED_STATE
    _UNCHANGED_STATE = None
    if os.geteuid() != 0:
        raise ValueError('root required')
    # Close old auth paths before touching config; failure never restarts them.
    stop_old_sshd()
    if not CONFIG_PATH.parent.exists():
        CONFIG_PATH.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
    protected_parent(CONFIG_PATH)
    try:
        pwd.getpwnam('boat-ssh')
    except KeyError:
        run('/usr/sbin/useradd', '--system', '--user-group', '--home-dir', '/nonexistent',
            '--no-create-home', '--shell', '/usr/sbin/nologin', 'boat-ssh')
    users, gid = check_accounts(config)
    archive_legacy([Path(pwd.getpwnam(user).pw_dir) / '.ssh' for user in users])
    ARCHIVE.mkdir(mode=0o700, exist_ok=True)
    protected_parent(ARCHIVE / 'check')
    policy_bytes = sshd_policy(config['users']).encode()
    if not exact_root_file(SSHD_CONFIG, policy_bytes, 0o644):
        if SSHD_CONFIG.exists() and not SSHD_CONFIG.is_symlink():
            old = SSHD_CONFIG.read_bytes()
            if old != policy_bytes:
                saved = ARCHIVE / ('sshd-' + hashlib.sha256(old).hexdigest() + '.conf')
                if not saved.exists():
                    atomic_file(saved, old, 0o600)
        atomic_file(SSHD_CONFIG, policy_bytes, 0o644)
    atomic_file(CONFIG_PATH, canonical(config), 0o640, gid)
    dropin_bytes = SYSTEMD_POLICY.encode()
    if not exact_root_file(DROPIN_PATH, dropin_bytes, 0o644):
        DROPIN_PATH.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
        atomic_file(DROPIN_PATH, dropin_bytes, 0o644)
        run('/usr/bin/systemctl', 'daemon-reload')
    ensure_sshd_run_dir()
    ensure_host_keys()
    verify_files_and_config(config)
    run('/usr/bin/systemctl', 'reset-failed', 'ssh.service', check=False)
    run('/usr/bin/systemctl', 'start', 'ssh.service')
    verify_service()


def verify_config_file(config, gid=None):
    if gid is None:
        try:
            gid = grp.getgrnam('boat-ssh').gr_gid
        except KeyError as error:
            raise ValueError('helper account') from error
    protected_parent(CONFIG_PATH)
    try:
        fd = os.open(CONFIG_PATH, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except OSError as error:
        raise ValueError('untrusted config') from error
    with os.fdopen(fd, 'rb') as stream:
        info = os.fstat(stream.fileno())
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or
                stat.S_IMODE(info.st_mode) != 0o640 or info.st_gid != gid):
            raise ValueError('untrusted config')
        data = stream.read(CONFIG_LIMIT + 1)
    if len(data) > CONFIG_LIMIT or data != canonical(config):
        raise ValueError('config mismatch')
    if validate_config(strict_json(data)) != config:
        raise ValueError('config mismatch')


def verify(config):
    global _UNCHANGED_STATE
    if os.geteuid() != 0:
        raise ValueError('root required')
    _UNCHANGED_STATE = None
    verify_policy(config)
    verify_service()


def verify_helper_digest(expected_sha256):
    if not isinstance(expected_sha256, str) or not re.fullmatch(r'[0-9a-f]{64}', expected_sha256):
        raise ValueError('invalid helper digest')
    protected_parent(HELPER_PATH)
    try:
        linfo = HELPER_PATH.lstat()
    except OSError as error:
        raise ValueError('helper missing') from error
    if (not stat.S_ISREG(linfo.st_mode) or stat.S_ISLNK(linfo.st_mode) or
            linfo.st_uid != 0 or linfo.st_gid != 0 or stat.S_IMODE(linfo.st_mode) != 0o555):
        raise ValueError('helper permissions')
    try:
        fd = os.open(HELPER_PATH, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except OSError as error:
        raise ValueError('helper open') from error
    with os.fdopen(fd, 'rb') as stream:
        info = os.fstat(stream.fileno())
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_gid != 0 or
                stat.S_IMODE(info.st_mode) != 0o555):
            raise ValueError('helper permissions')
        data = stream.read(HELPER_LIMIT + 1)
    if not data or len(data) > HELPER_LIMIT:
        raise ValueError('helper size')
    if hashlib.sha256(data).hexdigest() != expected_sha256:
        raise ValueError('helper digest mismatch')


_UNCHANGED_STATE = None


def _sshd_exe_matches(pid):
    try:
        return os.readlink('/proc/' + str(pid) + '/exe') == str(SSHD_BIN)
    except OSError:
        return False


def _policy_newest_mtime():
    mtimes = [SSHD_CONFIG.lstat().st_mtime, DROPIN_PATH.lstat().st_mtime]
    for key in HOST_KEYS:
        info = key.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_size == 0:
            raise ValueError('missing or invalid host key')
        mtimes.append(info.st_mtime)
    return max(mtimes)


def _fast_policy_verified(config):
    state = _UNCHANGED_STATE
    if state is None:
        return False
    pid, started, policy_bytes = state
    if policy_bytes != sshd_policy(config['users']).encode():
        return False
    if not SSHD_BIN.exists():
        return False
    try:
        if not _sshd_exe_matches(pid):
            return False
        cur_started = process_start_time(pid)
        return cur_started == started and cur_started > _policy_newest_mtime()
    except (OSError, ValueError, StopIteration, IndexError):
        return False


def install_and_verify(config, expected_helper_sha256=None):
    global _UNCHANGED_STATE
    if os.geteuid() != 0:
        raise ValueError('root required')
    if expected_helper_sha256 is not None:
        verify_helper_digest(expected_helper_sha256)
    _UNCHANGED_STATE = None
    if sshd_unchanged(config):
        try:
            refresh_config_only(config)
            # sshd_config, drop-in and unit are already current and the running sshd
            # postdates them: no restart, no sshd -t, single systemctl show, no redundant sshd -T.
            verify_policy(config)
            return
        finally:
            _UNCHANGED_STATE = None
    _UNCHANGED_STATE = None
    install(config)
    verify(config)


def process_start_time(pid):
    with open('/proc/stat', 'rb') as stream:
        btime = next(int(line.split()[1]) for line in stream if line.startswith(b'btime '))
    with open('/proc/' + str(pid) + '/stat', 'rb') as stream:
        raw = stream.read()
    fields = raw[raw.rindex(b')') + 2:].split()
    return btime + int(fields[19]) / os.sysconf('SC_CLK_TCK')


def sshd_unchanged(config):
    """True only when no restart is needed; any doubt falls back to the full install."""
    global _UNCHANGED_STATE
    _UNCHANGED_STATE = None
    try:
        check_accounts(config)
        protected_parent(HELPER_PATH)
        info = HELPER_PATH.lstat()
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_gid != 0 or
                stat.S_IMODE(info.st_mode) != 0o555):
            return False
        policy_bytes = sshd_policy(config['users']).encode()
        if not exact_root_file(SSHD_CONFIG, policy_bytes, 0o644):
            return False
        if not exact_root_file(DROPIN_PATH, SYSTEMD_POLICY.encode(), 0o644):
            return False
        pid = verify_service()  # also rejects an active ssh.socket and returns MainPID
        if not isinstance(pid, int):
            pid = int(run('/usr/bin/systemctl', 'show', '--property=MainPID', '--value', 'ssh.service').stdout.strip())
        if pid <= 1:
            return False
        newest = _policy_newest_mtime()
        started = process_start_time(pid)
        if started <= newest:
            return False
        _UNCHANGED_STATE = (pid, started, policy_bytes)
        return True
    except (OSError, ValueError, StopIteration, IndexError, subprocess.SubprocessError):
        return False


def refresh_config_only(config):
    # The JSON authority file is read per authorization by the helper; sshd needs no restart.
    users, gid = check_accounts(config)
    archive_legacy([Path(pwd.getpwnam(user).pw_dir) / '.ssh' for user in users])
    ARCHIVE.mkdir(mode=0o700, exist_ok=True)
    protected_parent(ARCHIVE / 'check')
    atomic_file(CONFIG_PATH, canonical(config), 0o640, gid)
    ensure_sshd_run_dir()


def rotate_token(config):
    if os.geteuid() != 0:
        raise ValueError('root required')
    validate_config(config)
    _, gid = check_accounts(config)
    if not CONFIG_PATH.parent.exists() or CONFIG_PATH.parent.is_symlink():
        raise ValueError('untrusted parent')
    protected_parent(CONFIG_PATH)
    if ARCHIVE.is_symlink():
        raise ValueError('archive permissions')
    if ARCHIVE.exists():
        protected_parent(ARCHIVE / 'check')
        archive_info = ARCHIVE.lstat()
        if (not stat.S_ISDIR(archive_info.st_mode) or archive_info.st_uid != 0 or
                stat.S_IMODE(archive_info.st_mode) != 0o700):
            raise ValueError('archive permissions')
    else:
        if not ARCHIVE.parent.exists() or ARCHIVE.parent.is_symlink():
            raise ValueError('untrusted parent')
        protected_parent(ARCHIVE)
    protected_parent(HELPER_PATH)
    try:
        helper_info = HELPER_PATH.lstat()
    except OSError as error:
        raise ValueError('helper permissions') from error
    if (not stat.S_ISREG(helper_info.st_mode) or helper_info.st_uid != 0 or
            helper_info.st_gid != 0 or stat.S_IMODE(helper_info.st_mode) != 0o555):
        raise ValueError('helper permissions')
    if not exact_root_file(SSHD_CONFIG, sshd_policy(config['users']).encode(), 0o644):
        raise ValueError('policy mismatch')
    if not exact_root_file(DROPIN_PATH, SYSTEMD_POLICY.encode(), 0o644):
        raise ValueError('dropin mismatch')
    try:
        verify_service()
    except subprocess.SubprocessError as error:
        raise ValueError('service inactive') from error
    atomic_file(CONFIG_PATH, canonical(config), 0o640, gid)
    verify_config_file(config, gid)


def timeout(_signum, _frame):
    raise TimeoutError('SSH authority deadline')


def main(argv=None):
    argv = sys.argv[1:] if argv is None else argv
    try:
        if len(argv) == 4 and argv[0] == 'authorize':
            old = signal.signal(signal.SIGALRM, timeout)
            signal.setitimer(signal.ITIMER_REAL, 2)
            try:
                line = lookup(read_config(), *argv[1:])
            finally:
                signal.setitimer(signal.ITIMER_REAL, 0)
                signal.signal(signal.SIGALRM, old)
            # Publish only a fully validated result, after disabling the timer.
            sys.stdout.write(line + '\n')
            sys.stdout.flush()
        elif len(argv) == 1 and argv[0] in ('install', 'verify', 'rotate-token', 'install-and-verify'):
            data = sys.stdin.buffer.read(CONFIG_LIMIT + 1)
            if len(data) > CONFIG_LIMIT:
                raise ValueError('config size')
            config = validate_config(strict_json(data))
            {'install': install, 'verify': verify, 'rotate-token': rotate_token,
             'install-and-verify': install_and_verify}[argv[0]](config)
            print(json.dumps(dict(version=1, config_digest=config_digest(config),
                                  boot_epoch=config['boot_epoch'], readiness=True), separators=(',', ':')))
        elif (argv[:1] == ['install-and-verify'] and
              ((len(argv) == 3 and argv[1] == '--expect-helper-sha256') or
               (len(argv) == 2 and argv[1].startswith('--expect-helper-sha256=')))):
            expected_sha = argv[2] if len(argv) == 3 else argv[1].split('=', 1)[1]
            if os.geteuid() != 0:
                raise ValueError('root required')
            verify_helper_digest(expected_sha)
            data = sys.stdin.buffer.read(CONFIG_LIMIT + 1)
            if len(data) > CONFIG_LIMIT:
                raise ValueError('config size')
            config = validate_config(strict_json(data))
            install_and_verify(config)
            print(json.dumps(dict(version=1, config_digest=config_digest(config),
                                  boot_epoch=config['boot_epoch'], readiness=True), separators=(',', ':')))
        else:
            raise ValueError('arguments')
        return 0
    except Exception:
        # Never log credentials, remote bodies, partial responses or tracebacks.
        return 1


if __name__ == '__main__':
    sys.exit(main())
