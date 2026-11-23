#!/usr/bin/env python3
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
from typing import NamedTuple

HOME = os.path.dirname(os.path.abspath(__file__))
TRUST = os.path.join(HOME, 'certificates', 'trust.pem')
TEST_STAGE = os.path.join(HOME, 'test')

PROFILE_SIGNER_SUBJECT = 'C=US,O=Apple Inc.,CN=Apple iPhone OS Provisioning Profile Signing'
PROFILE_SIGNER_ISSUER = 'C=US,O=Apple Inc.,OU=Certification Authority,CN=Apple iPhone Certification Authority'

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
DER_ENTITLEMENTS_MAGIC = 0xFADE7172
CMS_MAGIC = 0xFADE0B01
CODE_DIRECTORY_SLOT = 0
INFO_SLOT = 1
ENTITLEMENTS_SLOT = 5
DER_ENTITLEMENTS_SLOT = 7
CMS_SLOT = 0x10000
BLOB_HEADER_BYTES = 8
CODE_DIRECTORY_HEADER_BYTES = 40
ADHOC_FLAG = 0x2
TEAM_VERSION = 0x20200
TEAM_OFFSET = 48
HASHES = {1: ('sha1', 20), 2: ('sha256', 32), 3: ('sha256', 20), 4: ('sha384', 48)}

PEM_BEGIN = '-----BEGIN CERTIFICATE-----'
PEM_END = '-----END CERTIFICATE-----'


class Malformed(Exception):
    code = 2


class Invalid(Exception):
    code = 3


class Directory(NamedTuple):
    blob: bytes
    adhoc: bool
    team: str | None
    identifier: str
    hash_type: int
    hash_offset: int
    hash_size: int
    special_count: int


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


def trust():
    override = os.environ.get('DISTRIBUTION_TRUST')
    return override if override and os.path.isdir(TEST_STAGE) else TRUST


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
        if offset + BLOB_HEADER_BYTES > len(data):
            raise Invalid('code signature blob overruns the signature')
        magic, length = struct.unpack_from('>II', data, offset)
        if length < BLOB_HEADER_BYTES or offset + length > len(data):
            raise Invalid('code signature blob overruns the signature')
        if slot in slots:
            raise Invalid(f'code signature has two blobs in slot {slot}')
        slots[slot] = (magic, data[offset:offset + length])
    return slots


def c_string(blob, offset, name):
    end = blob.find(b'\0', offset)
    if offset >= len(blob) or end < 0:
        raise Invalid(f'CodeDirectory {name} overruns the blob')
    try:
        return blob[offset:end].decode('ascii')
    except UnicodeDecodeError:
        raise Invalid(f'CodeDirectory {name} is not ASCII') from None


def code_directory(blob):
    if len(blob) < CODE_DIRECTORY_HEADER_BYTES:
        raise Invalid('CodeDirectory is truncated')
    version, flags, hash_offset, identity_offset, special_count, _, _, hash_size, hash_type = struct.unpack_from(
        '>7I2B', blob, 8,
    )
    team = None
    if version >= TEAM_VERSION and len(blob) >= TEAM_OFFSET + 4:
        offset = struct.unpack_from('>I', blob, TEAM_OFFSET)[0]
        if offset:
            team = c_string(blob, offset, 'team identifier')
    return Directory(
        blob, bool(flags & ADHOC_FLAG), team, c_string(blob, identity_offset, 'identifier'),
        hash_type, hash_offset, hash_size, special_count,
    )


def special_hash(directory, slot):
    if slot > directory.special_count:
        return None
    start = directory.hash_offset - slot * directory.hash_size
    if start < 0 or directory.hash_offset > len(directory.blob):
        raise Invalid('CodeDirectory special slots overrun the blob')
    value = directory.blob[start:start + directory.hash_size]
    return None if value == bytes(directory.hash_size) else value


def digest(directory, data):
    name, size = HASHES[directory.hash_type]
    return hashlib.new(name, data).digest()[:size]


def bind(directory, slots, info_data, info):
    if directory.hash_type not in HASHES or HASHES[directory.hash_type][1] != directory.hash_size:
        raise Invalid(f'CodeDirectory hash type {directory.hash_type} with size {directory.hash_size} is not supported')
    if directory.identifier != text(info, 'CFBundleIdentifier'):
        raise Invalid(
            f'CodeDirectory identifier {directory.identifier} does not match CFBundleIdentifier {text(info, "CFBundleIdentifier")}',
        )
    if special_hash(directory, INFO_SLOT) != digest(directory, info_data):
        raise Invalid('the code signature does not seal this Info.plist')
    for slot, magic, name in (
        (ENTITLEMENTS_SLOT, ENTITLEMENTS_MAGIC, 'entitlements'),
        (DER_ENTITLEMENTS_SLOT, DER_ENTITLEMENTS_MAGIC, 'DER entitlements'),
    ):
        sealed = special_hash(directory, slot)
        found, blob = slots.get(slot, (None, None))
        if blob is None:
            if sealed is not None:
                raise Invalid(f'the code signature seals {name} that are missing')
            continue
        if found != magic:
            raise Invalid(f'code signature slot {slot} does not hold {name}')
        if sealed != digest(directory, blob):
            raise Invalid(f'the code signature does not seal these {name}')


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
    return completed.stdout.decode(errors='replace')


def only_certificate(path, what):
    with open(path, 'rb') as handle:
        text = handle.read().decode('ascii', errors='replace')
    certificates = []
    start = text.find(PEM_BEGIN)
    while start >= 0:
        end = text.find(PEM_END, start)
        if end < 0:
            break
        certificates.append(base64.b64decode(''.join(text[start + len(PEM_BEGIN):end].split())))
        start = text.find(PEM_BEGIN, end)
    if len(certificates) != 1:
        raise Invalid(f'{what} must have exactly one signer, found {len(certificates)}')
    return certificates[0]


def signer(cms, content, directory):
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
    return only_certificate(paths['signer.pem'], 'code signature CMS')


def profile(data, directory):
    envelope = os.path.join(directory, 'profile.der')
    content = os.path.join(directory, 'profile.plist')
    profile_signer = os.path.join(directory, 'profile-signer.pem')
    with open(envelope, 'wb') as handle:
        handle.write(data)
    anchors = trust()
    openssl(
        ['cms', '-verify', '-inform', 'der', '-in', envelope, '-binary',
         '-CAfile', anchors, '-purpose', 'any', '-signer', profile_signer, '-out', content],
        f'provisioning profile does not verify against {anchors}',
    )
    only_certificate(profile_signer, 'provisioning profile')
    names = openssl(
        ['x509', '-in', profile_signer, '-noout', '-subject', '-issuer', '-nameopt', 'RFC2253'],
        'provisioning profile signer certificate is unreadable',
    )
    if names != f'subject={PROFILE_SIGNER_SUBJECT}\nissuer={PROFILE_SIGNER_ISSUER}\n':
        raise Invalid(f"provisioning profile is not signed by Apple's profile signing certificate: {' '.join(names.split())}")
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


def covers(pattern, identifier):
    return pattern == identifier or (pattern.endswith('*') and identifier.startswith(pattern[:-1]))


def application_identifier(provision, team, identifier, entitlements):
    granted = provision.get('Entitlements')
    value = granted.get('application-identifier') if isinstance(granted, dict) else None
    if not isinstance(value, str) or '.' not in value:
        raise Invalid('provisioning profile has no application-identifier entitlement')
    prefix, pattern = value.split('.', 1)
    prefixes = provision.get('ApplicationIdentifierPrefix')
    allowed = {team, *(prefixes if isinstance(prefixes, list) else [])}
    if prefix not in allowed:
        raise Invalid(f'provisioning profile application-identifier {value} does not belong to team {team}')
    if not covers(pattern, identifier):
        raise Invalid(f'provisioning profile application-identifier {value} does not cover {identifier}')
    claimed = (entitlements or {}).get('application-identifier')
    if claimed is not None and claimed != f'{prefix}.{identifier}':
        raise Invalid(f'signed application-identifier {claimed} does not match {prefix}.{identifier}')


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

        info_data = bounded(archive, root + 'Info.plist', MAX_PLIST_BYTES)
        info = property_list(info_data, Malformed, 'Info.plist is not a property list')
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
            code = code_directory(directory_blob)
            adhoc, team = code.adhoc, code.team
            magic, blob = slots.get(ENTITLEMENTS_SLOT, (None, b''))
            if magic == ENTITLEMENTS_MAGIC:
                entitlements = property_list(blob[BLOB_HEADER_BYTES:], Invalid, 'entitlements are not a property list')
            magic, blob = slots.get(CMS_SLOT, (None, b''))
            cms = blob[BLOB_HEADER_BYTES:] if magic == CMS_MAGIC else b''
            if not adhoc:
                if not cms:
                    raise Invalid('signature is not ad hoc but carries no CMS signature')
                bind(code, slots, info_data, info)
                leaf = signer(cms, directory_blob, directory)

        provision = None
        if root + 'embedded.mobileprovision' in names:
            provision = profile(bounded(archive, root + 'embedded.mobileprovision', MAX_PROFILE_BYTES), directory)
            if leaf is not None:
                profile_team = provision['TeamIdentifier'][0]
                if team != profile_team:
                    raise Invalid(f'code signature team {team} does not match profile team {profile_team}')
                if leaf not in provision['DeveloperCertificates']:
                    raise Invalid("code signing certificate is not one of the profile's DeveloperCertificates")
                application_identifier(provision, team, text(info, 'CFBundleIdentifier'), entitlements)

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
    except Exception as error:
        print(f'inspect-ipa.py: malformed archive: {type(error).__name__}: {error}', file=sys.stderr)
        sys.exit(Malformed.code)
    print(json.dumps(result, default=serialise))


if __name__ == '__main__':
    main(sys.argv[1])
