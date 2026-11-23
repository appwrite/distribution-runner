#!/usr/bin/env python3
"""Inspects an uploaded .ipa without extracting it. Run by inspect.sh after its archive checks.

usage: python3 -I inspect-ipa.py <file.ipa>
env: DISTRIBUTION_TRUST (PEM bundle the provisioning profile must chain to)
exit: 2 malformed, 3 invalid signature, profile or team
"""
import base64
import datetime
import hashlib
import json
import os
import plistlib
import struct
import subprocess
import sys
import tempfile
import zipfile

TRUST = os.environ.get('DISTRIBUTION_TRUST') or '/usr/local/distribution/certificates/trust.pem'

MAX_PLIST_BYTES = 1 << 20
MAX_PROFILE_BYTES = 1 << 20
MAX_LOAD_COMMANDS_BYTES = 1 << 20
MAX_SIGNATURE_BYTES = 64 << 20
CHUNK_BYTES = 1 << 20

FAT_MAGIC = 0xCAFEBABE
FAT_MAGIC_64 = 0xCAFEBABF
THIN_MAGIC = 0xFEEDFACE
THIN_MAGIC_64 = 0xFEEDFACF
CODE_SIGNATURE_COMMAND = 0x1D

SUPERBLOB_MAGIC = 0xFADE0CC0
CODE_DIRECTORY_MAGIC = 0xFADE0C02
ENTITLEMENTS_MAGIC = 0xFADE7171
CMS_MAGIC = 0xFADE0B01
CODE_DIRECTORY_SLOT = 0
ENTITLEMENTS_SLOT = 5
CMS_SLOT = 0x10000
ADHOC_FLAG = 0x2
TEAM_VERSION = 0x20200
TEAM_OFFSET = 48

PEM_BEGIN = '-----BEGIN CERTIFICATE-----'
PEM_END = '-----END CERTIFICATE-----'


class Malformed(Exception):
    code = 2


class Invalid(Exception):
    code = 3


class Stream:
    def __init__(self, handle):
        self.handle = handle
        self.position = 0

    def read(self, size):
        data = self.handle.read(size)
        if len(data) != size:
            raise Malformed('executable is truncated')
        self.position += size
        return data

    def skip(self, offset):
        if offset < self.position:
            raise Malformed('executable offsets point backwards')
        while self.position < offset:
            self.read(min(CHUNK_BYTES, offset - self.position))


def bounded(archive, name, limit):
    try:
        info = archive.getinfo(name)
    except KeyError:
        raise Malformed(f'{name} is missing') from None
    if info.file_size > limit:
        raise Malformed(f'{name} declares {info.file_size} bytes, limit {limit}')
    with archive.open(info) as handle:
        data = handle.read(limit + 1)
    if len(data) > limit:
        raise Malformed(f'{name} inflates past {limit} bytes')
    return data


def code_signature(stream):
    """Returns the first slice's LC_CODE_SIGNATURE bytes, or None when the executable is unsigned."""
    head = stream.read(4)
    base = 0
    if struct.unpack('>I', head)[0] in (FAT_MAGIC, FAT_MAGIC_64):
        fat64 = struct.unpack('>I', head)[0] == FAT_MAGIC_64
        if struct.unpack('>I', stream.read(4))[0] == 0:
            raise Malformed('universal executable has no slices')
        base = struct.unpack('>8xQ' if fat64 else '>8xI', stream.read(16 if fat64 else 12))[0]
        stream.skip(base)
        head = stream.read(4)
    magic = struct.unpack('<I', head)[0]
    if magic not in (THIN_MAGIC, THIN_MAGIC_64):
        raise Malformed('executable is not a Mach-O file')
    header = stream.read((32 if magic == THIN_MAGIC_64 else 28) - 4)
    commands, commands_size = struct.unpack_from('<II', header, 12)
    if commands_size > MAX_LOAD_COMMANDS_BYTES:
        raise Malformed(f'load commands declare {commands_size} bytes, limit {MAX_LOAD_COMMANDS_BYTES}')
    load = stream.read(commands_size)
    cursor = 0
    for _ in range(commands):
        if cursor + 8 > len(load):
            raise Malformed('load commands overrun their declared size')
        command, size = struct.unpack_from('<II', load, cursor)
        if size < 8:
            raise Malformed('load command is too small')
        if command == CODE_SIGNATURE_COMMAND:
            if size < 16 or cursor + 16 > len(load):
                raise Malformed('LC_CODE_SIGNATURE is truncated')
            offset, length = struct.unpack_from('<II', load, cursor + 8)
            if length > MAX_SIGNATURE_BYTES:
                raise Malformed(f'code signature declares {length} bytes, limit {MAX_SIGNATURE_BYTES}')
            stream.skip(base + offset)
            return stream.read(length)
        cursor += size
    return None


def superblob(data):
    if len(data) < 12:
        raise Invalid('code signature is truncated')
    magic, _, count = struct.unpack_from('>III', data, 0)
    if magic != SUPERBLOB_MAGIC:
        raise Invalid('code signature is not an embedded signature superblob')
    if 12 + count * 8 > len(data):
        raise Invalid('code signature index overruns the signature')
    slots = {}
    for index in range(count):
        slot, offset = struct.unpack_from('>II', data, 12 + index * 8)
        if offset + 8 > len(data):
            raise Invalid('code signature blob overruns the signature')
        magic, length = struct.unpack_from('>II', data, offset)
        if length < 8 or offset + length > len(data):
            raise Invalid('code signature blob overruns the signature')
        slots[slot] = (magic, data[offset:offset + length])
    return slots


def code_directory(blob):
    """Returns (adhoc, team) from a CodeDirectory blob."""
    if len(blob) < 16:
        raise Invalid('CodeDirectory is truncated')
    version, flags = struct.unpack_from('>II', blob, 8)
    team = None
    if version >= TEAM_VERSION and len(blob) >= TEAM_OFFSET + 4:
        offset = struct.unpack_from('>I', blob, TEAM_OFFSET)[0]
        if offset:
            end = blob.find(b'\0', offset)
            if offset >= len(blob) or end < 0:
                raise Invalid('CodeDirectory team identifier overruns the blob')
            try:
                team = blob[offset:end].decode('ascii')
            except UnicodeDecodeError:
                raise Invalid('CodeDirectory team identifier is not ASCII') from None
    return bool(flags & ADHOC_FLAG), team


def property_list(data, error, reason):
    try:
        value = plistlib.loads(data)
    except Exception:
        raise error(reason) from None
    if not isinstance(value, dict):
        raise error(reason)
    return value


def openssl(arguments, reason):
    completed = subprocess.run(['openssl', *arguments], capture_output=True, check=False)
    if completed.returncode != 0:
        sys.stderr.write(completed.stderr.decode(errors='replace'))
        raise Invalid(reason)


def certificate(pem):
    text = pem.decode('ascii', errors='replace')
    start = text.find(PEM_BEGIN)
    end = text.find(PEM_END, start)
    if start < 0 or end < 0:
        raise Invalid('code signature CMS has no signer certificate')
    return base64.b64decode(''.join(text[start + len(PEM_BEGIN):end].split()))


def signer(cms, content, directory):
    """Verifies the detached CMS signature over the CodeDirectory and returns the signer certificate (DER)."""
    paths = {name: os.path.join(directory, name) for name in ('signature.der', 'directory.bin', 'signer.pem')}
    with open(paths['signature.der'], 'wb') as handle:
        handle.write(cms)
    with open(paths['directory.bin'], 'wb') as handle:
        handle.write(content)
    openssl(
        ['cms', '-verify', '-inform', 'der', '-in', paths['signature.der'], '-binary',
         '-content', paths['directory.bin'], '-noverify', '-signer', paths['signer.pem'], '-out', os.devnull],
        'code signature CMS does not verify over the CodeDirectory',
    )
    with open(paths['signer.pem'], 'rb') as handle:
        return certificate(handle.read())


def profile(data, directory):
    envelope = os.path.join(directory, 'profile.der')
    content = os.path.join(directory, 'profile.plist')
    with open(envelope, 'wb') as handle:
        handle.write(data)
    openssl(
        ['cms', '-verify', '-inform', 'der', '-in', envelope, '-binary',
         '-CAfile', TRUST, '-purpose', 'any', '-out', content],
        f'provisioning profile does not verify against {TRUST}',
    )
    with open(content, 'rb') as handle:
        parsed = property_list(handle.read(MAX_PROFILE_BYTES + 1), Invalid, 'provisioning profile is not a property list')
    teams = parsed.get('TeamIdentifier')
    if not isinstance(teams, list) or not teams or not isinstance(teams[0], str):
        raise Invalid('provisioning profile has no TeamIdentifier')
    if not isinstance(parsed.get('ExpirationDate'), datetime.datetime):
        raise Invalid('provisioning profile has no ExpirationDate')
    certificates = parsed.get('DeveloperCertificates')
    if not isinstance(certificates, list) or not all(isinstance(entry, bytes) for entry in certificates):
        raise Invalid('provisioning profile has no DeveloperCertificates')
    return parsed


def text(info, key):
    value = info.get(key)
    return value if isinstance(value, str) else None


def serialise(value):
    if isinstance(value, datetime.datetime):
        return value.strftime('%Y-%m-%dT%H:%M:%SZ')
    if isinstance(value, bytes):
        return base64.b64encode(value).decode('ascii')
    raise TypeError(f'cannot serialise {type(value).__name__}')


def inspect(path, directory):
    with zipfile.ZipFile(path) as archive:
        names = set(archive.namelist())
        apps = sorted({
            name.split('/')[1] for name in names
            if name.startswith('Payload/') and name.count('/') >= 2 and name.split('/')[1].endswith('.app')
        })
        if len(apps) != 1:
            raise Malformed(f'expected exactly one Payload/*.app, found {len(apps)}')
        root = f'Payload/{apps[0]}/'

        info = property_list(bounded(archive, root + 'Info.plist', MAX_PLIST_BYTES), Malformed, 'Info.plist is not a property list')
        executable = text(info, 'CFBundleExecutable')
        if not executable or '/' in executable or executable in ('.', '..'):
            raise Malformed('Info.plist has no valid CFBundleExecutable')
        if root + executable not in names:
            raise Malformed(f'executable {executable} is missing')
        with archive.open(root + executable) as handle:
            signature = code_signature(Stream(handle))

        adhoc, team, entitlements, leaf = False, None, None, None
        if signature is not None:
            slots = superblob(signature)
            magic, directory_blob = slots.get(CODE_DIRECTORY_SLOT, (None, b''))
            if magic != CODE_DIRECTORY_MAGIC:
                raise Invalid('code signature has no CodeDirectory')
            adhoc, team = code_directory(directory_blob)
            magic, blob = slots.get(ENTITLEMENTS_SLOT, (None, b''))
            if magic == ENTITLEMENTS_MAGIC:
                entitlements = property_list(blob[8:], Invalid, 'entitlements are not a property list')
            magic, blob = slots.get(CMS_SLOT, (None, b''))
            cms = blob[8:] if magic == CMS_MAGIC else b''
            if not adhoc:
                if not cms:
                    raise Invalid('signature is not ad hoc but carries no CMS signature')
                leaf = signer(cms, directory_blob, directory)

        provision = None
        if root + 'embedded.mobileprovision' in names:
            provision = profile(bounded(archive, root + 'embedded.mobileprovision', MAX_PROFILE_BYTES), directory)
            if leaf is not None:
                if team != provision['TeamIdentifier'][0]:
                    raise Invalid(f'code signature team {team} does not match profile team {provision["TeamIdentifier"][0]}')
                if leaf not in provision['DeveloperCertificates']:
                    raise Invalid('code signing certificate is not one of the profile\'s DeveloperCertificates')

    return {
        'type': 'ipa',
        'identifier': text(info, 'CFBundleIdentifier'),
        'versionName': text(info, 'CFBundleShortVersionString'),
        'versionCode': text(info, 'CFBundleVersion'),
        'signed': signature is not None and not adhoc and provision is not None,
        'fingerprint': hashlib.sha256(leaf).hexdigest() if leaf is not None else None,
        'minSdk': None,
        'targetSdk': None,
        'entitlements': entitlements,
        'profileExpiresAt': serialise(provision['ExpirationDate']) if provision is not None else None,
        'teamId': provision['TeamIdentifier'][0] if provision is not None else team,
    }


def main(path):
    try:
        with tempfile.TemporaryDirectory() as directory:
            result = inspect(path, directory)
    except (Malformed, Invalid) as error:
        print(f'inspect-ipa.py: {error}', file=sys.stderr)
        sys.exit(error.code)
    except (OSError, EOFError, zipfile.BadZipFile, struct.error, ValueError) as error:
        print(f'inspect-ipa.py: malformed archive: {error}', file=sys.stderr)
        sys.exit(2)
    print(json.dumps(result, default=serialise))


if __name__ == '__main__':
    main(sys.argv[1])
