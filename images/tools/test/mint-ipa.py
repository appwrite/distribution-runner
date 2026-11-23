#!/usr/bin/env python3
"""TEST ONLY: mints a synthetic signed .ipa whose profile and code signature chain to the test CA.

usage: python3 -I mint-ipa.py --identifier ID --version-name 1.0 --version-code 1 --team ABCDE12345 --out app.ipa
"""
import argparse
import datetime
import hashlib
import os
import plistlib
import struct
import subprocess
import sys
import tempfile
import uuid
import zipfile

HERE = os.path.dirname(os.path.abspath(__file__))
PAGE_SIZE = 4096
CODE_PAGES = 2
SIGNATURE_RESERVED_BYTES = 32768
TIMESTAMP = (2026, 1, 1, 0, 0, 0)

SUPERBLOB_MAGIC = 0xFADE0CC0
CODE_DIRECTORY_MAGIC = 0xFADE0C02
REQUIREMENTS_MAGIC = 0xFADE0C01
ENTITLEMENTS_MAGIC = 0xFADE7171
CMS_MAGIC = 0xFADE0B01
CODE_DIRECTORY_SLOT = 0
REQUIREMENTS_SLOT = 2
ENTITLEMENTS_SLOT = 5
CMS_SLOT = 0x10000
CODE_DIRECTORY_VERSION = 0x20400
CODE_DIRECTORY_HEADER_BYTES = 88
ADHOC_FLAG = 0x2
SHA256_HASH_TYPE = 2
MAIN_BINARY_FLAG = 0x1
SPECIAL_SLOTS = 5

THIN_MAGIC_64 = 0xFEEDFACF
ARM64_CPU_TYPE = 0x0100000C
EXECUTE_FILE_TYPE = 0x2
HEADER_FLAGS = 0x00200085
CODE_SIGNATURE_COMMAND = 0x1D
ARM64_NOP = b'\x1f\x20\x03\xd5'


def openssl(*arguments):
    completed = subprocess.run(['openssl', *arguments], capture_output=True, check=False)
    if completed.returncode != 0:
        sys.stderr.write(completed.stderr.decode(errors='replace'))
        raise SystemExit(f'mint-ipa.py: openssl {arguments[0]} failed')
    return completed.stdout


def issue(directory, name, subject, authority, usage):
    key = os.path.join(directory, f'{name}.key')
    certificate = os.path.join(directory, f'{name}.pem')
    openssl(
        'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-sha256', '-days', '365',
        '-keyout', key, '-out', certificate, '-subj', subject,
        '-CA', authority.certificate, '-CAkey', authority.key,
        '-addext', 'basicConstraints=critical,CA:FALSE',
        '-addext', 'keyUsage=critical,digitalSignature',
        '-addext', f'extendedKeyUsage={usage}',
    )
    return key, certificate


def sign(directory, name, content, key, certificate, authority, detached):
    source = os.path.join(directory, f'{name}.in')
    with open(source, 'wb') as handle:
        handle.write(content)
    arguments = [
        'cms', '-sign', '-binary', '-md', 'sha256', '-in', source, '-signer', certificate, '-inkey', key,
        '-certfile', authority.certificate, '-outform', 'der',
    ]
    if not detached:
        arguments.append('-nodetach')
    return openssl(*arguments)


def entitlements(team, identifier):
    return {
        'application-identifier': f'{team}.{identifier}',
        'beta-reports-active': True,
        'com.apple.developer.team-identifier': team,
        'get-task-allow': False,
        'keychain-access-groups': [f'{team}.*'],
    }


def blob(magic, payload):
    return struct.pack('>II', magic, 8 + len(payload)) + payload


def code_directory(identifier, team, adhoc, code, special):
    identity = identifier.encode() + b'\0'
    team_bytes = b'' if adhoc else team.encode() + b'\0'
    identity_offset = CODE_DIRECTORY_HEADER_BYTES
    team_offset = 0 if adhoc else identity_offset + len(identity)
    hash_offset = identity_offset + len(identity) + len(team_bytes) + SPECIAL_SLOTS * 32
    pages = [hashlib.sha256(code[start:start + PAGE_SIZE]).digest() for start in range(0, len(code), PAGE_SIZE)]
    hashes = b''.join(special.get(slot, b'\0' * 32) for slot in range(SPECIAL_SLOTS, 0, -1)) + b''.join(pages)
    length = hash_offset + len(pages) * 32
    header = struct.pack(
        '>9I4B4I4Q',
        CODE_DIRECTORY_MAGIC, length, CODE_DIRECTORY_VERSION, ADHOC_FLAG if adhoc else 0,
        hash_offset, identity_offset, SPECIAL_SLOTS, len(pages), len(code),
        32, SHA256_HASH_TYPE, 0, 12,
        0, 0, team_offset, 0,
        0, 0, len(code), MAIN_BINARY_FLAG,
    )
    return header + identity + team_bytes + hashes


def superblob(blobs):
    offset = 12 + 8 * len(blobs)
    index = b''
    for slot, content in blobs:
        index += struct.pack('>II', slot, offset)
        offset += len(content)
    return struct.pack('>III', SUPERBLOB_MAGIC, offset, len(blobs)) + index + b''.join(content for _, content in blobs)


def executable(options, directory, authority, leaf, info, resources, entitlement):
    header = struct.pack(
        '<IiiIIIII', THIN_MAGIC_64, ARM64_CPU_TYPE, 0, EXECUTE_FILE_TYPE, 1, 16, HEADER_FLAGS, 0,
    )
    signature_offset = CODE_PAGES * PAGE_SIZE
    command = struct.pack('<IIII', CODE_SIGNATURE_COMMAND, 16, signature_offset, SIGNATURE_RESERVED_BYTES)
    prefix = header + command
    code = prefix + ARM64_NOP * ((signature_offset - len(prefix)) // len(ARM64_NOP))

    requirements = struct.pack('>III', REQUIREMENTS_MAGIC, 12, 0)
    entitlements_blob = blob(ENTITLEMENTS_MAGIC, entitlement)
    special = {
        1: hashlib.sha256(info).digest(),
        2: hashlib.sha256(requirements).digest(),
        3: hashlib.sha256(resources).digest(),
        5: hashlib.sha256(entitlements_blob).digest(),
    }
    directory_blob = code_directory(options.identifier, options.team, options.adhoc, code, special)
    if options.adhoc:
        cms = b''
    else:
        cms = sign(directory, 'directory', directory_blob, leaf[0], leaf[1], authority, detached=True)
    signature = superblob([
        (CODE_DIRECTORY_SLOT, directory_blob),
        (REQUIREMENTS_SLOT, requirements),
        (ENTITLEMENTS_SLOT, entitlements_blob),
        (CMS_SLOT, blob(CMS_MAGIC, cms)),
    ])
    if len(signature) > SIGNATURE_RESERVED_BYTES:
        raise SystemExit('mint-ipa.py: code signature does not fit the reserved space')
    return code + signature + b'\0' * (SIGNATURE_RESERVED_BYTES - len(signature))


def expiry(value):
    if value is None:
        return datetime.datetime.now(datetime.UTC).replace(microsecond=0, tzinfo=None) + datetime.timedelta(days=365)
    parsed = datetime.datetime.fromisoformat(value)
    if parsed.tzinfo is not None:
        parsed = parsed.astimezone(datetime.UTC).replace(tzinfo=None)
    return parsed.replace(microsecond=0)


def arguments():
    parser = argparse.ArgumentParser(description='TEST ONLY: mint a synthetic signed .ipa chained to the test CA.')
    parser.add_argument('--identifier', required=True, help='CFBundleIdentifier')
    parser.add_argument('--version-name', default='1.0', help='CFBundleShortVersionString')
    parser.add_argument('--version-code', default='1', help='CFBundleVersion')
    parser.add_argument('--team', default='ABCDE12345', help='team identifier in the code signature')
    parser.add_argument('--profile-team', help='team identifier in the provisioning profile (default: --team)')
    parser.add_argument('--name', default='App', help='executable and .app name')
    parser.add_argument('--adhoc', action='store_true', help='sign ad hoc: no team, no CMS signature')
    parser.add_argument('--no-profile', action='store_true', help='omit embedded.mobileprovision')
    parser.add_argument('--expires', help='profile ExpirationDate, ISO 8601 (default: one year from now)')
    parser.add_argument('--ca-cert', default=os.path.join(HERE, 'ca.pem'), help='issuing CA certificate (PEM)')
    parser.add_argument('--ca-key', default=os.path.join(HERE, 'ca.key'), help='issuing CA private key (PEM)')
    parser.add_argument('--leaf-out', help='write the code signing certificate (PEM) here')
    parser.add_argument('--out', required=True, help='path of the .ipa to write')
    options = parser.parse_args()
    options.profile_team = options.profile_team or options.team
    for name in ('identifier', 'team', 'profile_team', 'name'):
        value = getattr(options, name)
        if not value or not value.isascii() or '\0' in value or '/' in value:
            parser.error(f'--{name.replace("_", "-")} must be non-empty ASCII without "/"')
    return options


class Authority:
    def __init__(self, certificate, key):
        self.certificate = certificate
        self.key = key


def mint(options, directory):
    authority = Authority(options.ca_cert, options.ca_key)
    leaf = issue(
        directory, 'distribution', f'/C=US/O=Appwrite Distribution TEST ONLY/OU={options.team}'
        f'/CN=Apple Distribution: Appwrite Distribution Test ({options.team})', authority, 'codeSigning',
    )
    profile_signer = issue(
        directory, 'profile', '/C=US/O=Appwrite Distribution TEST ONLY/CN=Test Provisioning Profile Signing',
        authority, 'codeSigning',
    )
    leaf_der = openssl('x509', '-in', leaf[1], '-outform', 'der')

    now = datetime.datetime.now(datetime.UTC).replace(microsecond=0, tzinfo=None)
    profile = sign(directory, 'profile', plistlib.dumps({
        'AppIDName': 'Appwrite Distribution Test',
        'ApplicationIdentifierPrefix': [options.profile_team],
        'CreationDate': now,
        'DeveloperCertificates': [leaf_der],
        'Entitlements': entitlements(options.profile_team, options.identifier),
        'ExpirationDate': expiry(options.expires),
        'IsXcodeManaged': False,
        'Name': f'Appwrite Distribution Test {options.identifier}',
        'Platform': ['iOS'],
        'TeamIdentifier': [options.profile_team],
        'TeamName': 'Appwrite Distribution Test',
        'TimeToLive': 365,
        'UUID': str(uuid.uuid4()).upper(),
        'Version': 1,
    }), profile_signer[0], profile_signer[1], authority, detached=False)

    info = plistlib.dumps({
        'CFBundleExecutable': options.name,
        'CFBundleIdentifier': options.identifier,
        'CFBundleName': options.name,
        'CFBundlePackageType': 'APPL',
        'CFBundleShortVersionString': options.version_name,
        'CFBundleSupportedPlatforms': ['iPhoneOS'],
        'CFBundleVersion': options.version_code,
        'MinimumOSVersion': '15.0',
        'UIDeviceFamily': [1, 2],
    }, fmt=plistlib.FMT_BINARY)

    files = {'Info.plist': info}
    if not options.no_profile:
        files['embedded.mobileprovision'] = profile
    resources = plistlib.dumps({
        'files': {name: hashlib.sha1(content).digest() for name, content in files.items()},
        'files2': {name: {'hash2': hashlib.sha256(content).digest()} for name, content in files.items()},
    })
    entitlement = plistlib.dumps(entitlements(options.team, options.identifier))
    files[options.name] = executable(options, directory, authority, leaf, info, resources, entitlement)
    files['_CodeSignature/CodeResources'] = resources

    root = f'Payload/{options.name}.app/'
    partial = os.path.join(os.path.dirname(os.path.abspath(options.out)), f'.{os.path.basename(options.out)}.partial')
    with zipfile.ZipFile(partial, 'w', zipfile.ZIP_DEFLATED) as archive:
        for name, content in files.items():
            entry = zipfile.ZipInfo(root + name, TIMESTAMP)
            entry.compress_type = zipfile.ZIP_DEFLATED
            entry.external_attr = (0o755 if name == options.name else 0o644) << 16
            archive.writestr(entry, content)
    os.replace(partial, options.out)

    if options.leaf_out:
        with open(leaf[1], 'rb') as source, open(options.leaf_out, 'wb') as target:
            target.write(source.read())


def main():
    options = arguments()
    with tempfile.TemporaryDirectory() as directory:
        mint(options, directory)


if __name__ == '__main__':
    main()
