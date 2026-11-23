#!/usr/bin/env python3
import plistlib
import struct
import sys
import warnings
import zipfile

END_OF_DIRECTORY = b'PK\x05\x06'
ZIP64_LOCATOR = b'PK\x06\x07'
DIRECTORY_HEADER_BYTES = 46
LOCAL_FLAGS = 6
LOCAL_METHOD = 8
CENTRAL_FLAGS = 8
CENTRAL_METHOD = 10
ENCRYPTED_FLAG = 0x1
SHRUNK_METHOD = 1
DER_ENTITLEMENTS_MAGIC = b'\xfa\xde\x71\x72'
ENTITLEMENT_KEY = b'beta-reports-active'
SWAPPED_IDENTIFIER = 'io.appwrite.distribution.swapped'
EXTRA_ENTRIES = 50
TRUNCATED_EXECUTABLE_BYTES = 2000
UNREACHABLE_OFFSET = (1 << 64) - 1

VERIFIED = 'jar verified.'
SIGNER = '- Signed by '
UNSIGNED_WARNING = 'This jar contains unsigned entries which have not been integrity-checked.'


def entries(path):
    with zipfile.ZipFile(path) as archive:
        return [(info, archive.read(info)) for info in archive.infolist()]


def write(path, items, stored=()):
    with warnings.catch_warnings():
        warnings.simplefilter('ignore')
        with zipfile.ZipFile(path, 'w') as archive:
            for info, data in items:
                clone = zipfile.ZipInfo(info.filename, info.date_time)
                clone.external_attr = info.external_attr
                clone.compress_type = zipfile.ZIP_STORED if info.filename in stored else zipfile.ZIP_DEFLATED
                archive.writestr(clone, data)


def application(items, suffix):
    return next(info.filename for info, _ in items if info.filename.startswith('Payload/') and info.filename.endswith(suffix))


def executable_name(items):
    info_plist = application(items, '.app/Info.plist')
    data = next(data for info, data in items if info.filename == info_plist)
    return info_plist.rsplit('/', 1)[0] + '/' + plistlib.loads(data)['CFBundleExecutable']


def replace(items, name, change):
    return [(info, change(data) if info.filename == name else data) for info, data in items]


def flip(data, start):
    position = data.index(ENTITLEMENT_KEY, start) + len(ENTITLEMENT_KEY) - 1
    return data[:position] + bytes([data[position] ^ 1]) + data[position + 1:]


def patch_headers(path, name, local_field, central_field, value):
    data = bytearray(open(path, 'rb').read())
    with zipfile.ZipFile(path) as archive:
        struct.pack_into('<H', data, archive.getinfo(name).header_offset + local_field, value)
    end = data.rfind(END_OF_DIRECTORY)
    size, offset = struct.unpack_from('<II', data, end + 12)
    cursor = offset
    while cursor < offset + size:
        name_length, extra, comment = struct.unpack_from('<HHH', data, cursor + 28)
        if data[cursor + DIRECTORY_HEADER_BYTES:cursor + DIRECTORY_HEADER_BYTES + name_length].decode() == name:
            struct.pack_into('<H', data, cursor + central_field, value)
        cursor += DIRECTORY_HEADER_BYTES + name_length + extra + comment
    open(path, 'wb').write(bytes(data))


def archive_mode(mode, source, target):
    items = entries(source)
    if mode == 'info-identifier':
        name = application(items, '.app/Info.plist')

        def swap(data):
            info = plistlib.loads(data)
            info['CFBundleIdentifier'] = SWAPPED_IDENTIFIER
            return plistlib.dumps(info, fmt=plistlib.FMT_BINARY)

        write(target, replace(items, name, swap))
    elif mode == 'entitlements':
        write(target, replace(items, executable_name(items), lambda data: flip(data, 0)))
    elif mode == 'der-entitlements':
        write(target, replace(items, executable_name(items), lambda data: flip(data, data.index(DER_ENTITLEMENTS_MAGIC))))
    elif mode == 'truncate-executable':
        write(target, replace(items, executable_name(items), lambda data: data[:TRUNCATED_EXECUTABLE_BYTES]))
    elif mode == 'drop-executable':
        executable = executable_name(items)
        write(target, [(info, data) for info, data in items if info.filename != executable])
    elif mode in ('shrink-info', 'encrypt-info', 'corrupt-info'):
        name = application(items, '.app/Info.plist')
        write(target, items, stored=() if mode == 'corrupt-info' else (name,))
        if mode == 'shrink-info':
            patch_headers(target, name, LOCAL_METHOD, CENTRAL_METHOD, SHRUNK_METHOD)
        elif mode == 'encrypt-info':
            patch_headers(target, name, LOCAL_FLAGS, CENTRAL_FLAGS, ENCRYPTED_FLAG)
        else:
            data = bytearray(open(target, 'rb').read())
            with zipfile.ZipFile(target) as archive:
                info = archive.getinfo(name)
            start = info.header_offset + 30 + len(info.filename.encode()) + len(info.extra)
            for position in range(start, start + info.compress_size):
                data[position] ^= 0x55
            open(target, 'wb').write(bytes(data))
    elif mode == 'lying-count':
        padding = [(zipfile.ZipInfo(f'padding/{index}', (2026, 1, 1, 0, 0, 0)), b'') for index in range(EXTRA_ENTRIES)]
        write(target, items + padding)
        data = bytearray(open(target, 'rb').read())
        end = data.rfind(END_OF_DIRECTORY)
        struct.pack_into('<HH', data, end + 8, 1, 1)
        open(target, 'wb').write(bytes(data))
    elif mode == 'zip64-locator':
        write(target, items)
        data = open(target, 'rb').read()
        end = data.rfind(END_OF_DIRECTORY)
        record = bytearray(data[end:])
        struct.pack_into('<HH', record, 8, 0xFFFF, 0xFFFF)
        locator = ZIP64_LOCATOR + struct.pack('<IQI', 0, UNREACHABLE_OFFSET, 1)
        open(target, 'wb').write(data[:end] + locator + bytes(record))
    elif mode == 'duplicate':
        write(target, items + [items[-1]])
    elif mode == 'lowercase-signature':
        write(target, items + [(zipfile.ZipInfo('meta-inf/upload.rsa', (2026, 1, 1, 0, 0, 0)), b'signature')])
    elif mode == 'not-zip':
        open(target, 'wb').write(b'not a zip archive\n' * 64)
    else:
        raise SystemExit(f'tamper.py: unknown archive mode {mode}')


def log_mode(mode):
    lines = sys.stdin.read().splitlines()
    if mode == 'unverified':
        lines = [line for line in lines if line != VERIFIED]
    elif mode == 'two-signers':
        lines = [repeated for line in lines for repeated in ([line, line] if line.startswith(SIGNER) else [line])]
    elif mode == 'digest':
        lines = [line.replace('Digest algorithm: SHA-256', 'Digest algorithm: SHA-1') for line in lines]
    elif mode == 'algorithm':
        lines = [line.replace('Signature algorithm: SHA256withRSA', 'Signature algorithm: SHA1withRSA') for line in lines]
    elif mode == 'unsigned':
        lines.append(f'Warning: {UNSIGNED_WARNING}')
    else:
        raise SystemExit(f'tamper.py: unknown log mode {mode}')
    print('\n'.join(lines))


if __name__ == '__main__':
    if sys.argv[1] == 'log':
        log_mode(sys.argv[2])
    else:
        archive_mode(*sys.argv[1:4])
