#!/usr/bin/env python3
import argparse
import os
import struct
import sys
import zipfile

END_OF_DIRECTORY = b'PK\x05\x06'
ZIP64_LOCATOR = b'PK\x06\x07'
ZIP64_END_OF_DIRECTORY = b'PK\x06\x06'
DIRECTORY_HEADER = b'PK\x01\x02'
END_OF_DIRECTORY_BYTES = 22
ZIP64_LOCATOR_BYTES = 20
ZIP64_END_OF_DIRECTORY_BYTES = 56
DIRECTORY_HEADER_BYTES = 46
MAX_COMMENT_BYTES = 0xFFFF
ZIP64_MARKER_16 = 0xFFFF
ZIP64_MARKER_32 = 0xFFFFFFFF
ENCRYPTED_FLAG = 0x1
COMPRESSION_METHODS = (zipfile.ZIP_STORED, zipfile.ZIP_DEFLATED)

MANIFEST = 'base/manifest/AndroidManifest.xml'
SIGNATURE_DIRECTORY = 'META-INF/'
SIGNATURE_EXTENSIONS = ('.SF', '.RSA', '.DSA', '.EC')
SIGNATURE_PREFIX = 'SIG-'

CEILINGS = {
    'DISTRIBUTION_MAX_ENTRIES': 100_000,
    'DISTRIBUTION_MAX_DIRECTORY_BYTES': 64 << 20,
    'DISTRIBUTION_MAX_UNCOMPRESSED_BYTES': 16_000_000_000,
    'DISTRIBUTION_MAX_MANIFEST_BYTES': 8 << 20,
}


class Rejected(Exception):
    pass


class Usage(Exception):
    pass


def ceiling(name):
    value = os.environ.get(name) or str(CEILINGS[name])
    if not (value.isascii() and value.isdigit()) or value.startswith('0'):
        raise Usage(f'{name} must be a positive integer, got {value!r}')
    return int(value)


def is_signature(name):
    name = name.upper()
    if not name.startswith(SIGNATURE_DIRECTORY):
        return False
    file = name[len(SIGNATURE_DIRECTORY):]
    return '/' not in file and (file.endswith(SIGNATURE_EXTENSIONS) or file.startswith(SIGNATURE_PREFIX))


def end_records(handle, size):
    window = min(size, MAX_COMMENT_BYTES + END_OF_DIRECTORY_BYTES)
    handle.seek(size - window)
    tail = handle.read(window)
    position = tail.rfind(END_OF_DIRECTORY)
    if position < 0 or position + END_OF_DIRECTORY_BYTES > len(tail):
        raise Rejected('not a zip archive: no end of central directory record')
    end = size - window + position
    entries, length, offset = struct.unpack_from('<HII', tail, position + 10)
    if entries != ZIP64_MARKER_16 and ZIP64_MARKER_32 not in (length, offset):
        return end, entries, length, offset
    locator = position - ZIP64_LOCATOR_BYTES
    if locator < 0 or tail[locator:locator + 4] != ZIP64_LOCATOR:
        return end, entries, length, offset
    record = struct.unpack_from('<Q', tail, locator + 8)[0]
    if record + ZIP64_END_OF_DIRECTORY_BYTES > size:
        raise Rejected('zip64 end of central directory record lies outside the file')
    handle.seek(record)
    data = handle.read(ZIP64_END_OF_DIRECTORY_BYTES)
    if len(data) < ZIP64_END_OF_DIRECTORY_BYTES or data[:4] != ZIP64_END_OF_DIRECTORY:
        raise Rejected('malformed zip64 end of central directory record')
    entries, length, offset = struct.unpack_from('<QQQ', data, 32)
    return record, entries, length, offset


def count_directory(handle, size):
    end, declared, length, offset = end_records(handle, size)
    limit = ceiling('DISTRIBUTION_MAX_DIRECTORY_BYTES')
    if length > limit:
        raise Rejected(f'central directory is {length} bytes, limit {limit}')
    if offset + length != end:
        raise Rejected('central directory is not where the end of central directory record says')
    handle.seek(offset)
    directory = handle.read(length)
    if len(directory) != length:
        raise Rejected('central directory is truncated')
    maximum = ceiling('DISTRIBUTION_MAX_ENTRIES')
    if declared > maximum:
        raise Rejected(f'too many entries, limit {maximum}')
    found = 0
    cursor = 0
    while cursor < length:
        if cursor + DIRECTORY_HEADER_BYTES > length or directory[cursor:cursor + 4] != DIRECTORY_HEADER:
            raise Rejected('malformed central directory header')
        name, extra, comment = struct.unpack_from('<HHH', directory, cursor + 28)
        cursor += DIRECTORY_HEADER_BYTES + name + extra + comment
        found += 1
        if found > maximum:
            raise Rejected(f'too many entries, limit {maximum}')
    if cursor != length:
        raise Rejected('central directory overruns its declared size')
    if found != declared:
        raise Rejected(f'end of central directory record declares {declared} entries, the directory holds {found}')


def inspect(path, kind):
    with open(path, 'rb') as handle:
        handle.seek(0, os.SEEK_END)
        count_directory(handle, handle.tell())

    with zipfile.ZipFile(path) as archive:
        entries = archive.infolist()

    names = [entry.filename for entry in entries]
    if len(set(names)) != len(names):
        raise Rejected('duplicate entry names')
    for entry in entries:
        if entry.flag_bits & ENCRYPTED_FLAG:
            raise Rejected(f'{entry.filename} is encrypted')
        if entry.compress_type not in COMPRESSION_METHODS:
            raise Rejected(f'{entry.filename} uses unsupported compression method {entry.compress_type}')
    total = sum(entry.file_size for entry in entries)
    limit = ceiling('DISTRIBUTION_MAX_UNCOMPRESSED_BYTES')
    if total > limit:
        raise Rejected(f'uncompressed size {total} exceeds {limit}')
    if kind == 'aab':
        manifest = next((entry for entry in entries if entry.filename == MANIFEST), None)
        if manifest is None:
            raise Rejected(f'{MANIFEST} is missing')
        limit = ceiling('DISTRIBUTION_MAX_MANIFEST_BYTES')
        if manifest.file_size > limit:
            raise Rejected(f'{MANIFEST} declares {manifest.file_size} bytes, limit {limit}')
    return sum(1 for name in names if is_signature(name))


def main():
    parser = argparse.ArgumentParser(description='Check a hostile zip archive against the ceilings before anything parses it.')
    parser.add_argument('--malformed', type=int, required=True, help='exit code for a malformed or oversized archive')
    parser.add_argument('--usage', type=int, required=True, help='exit code for an invalid ceiling')
    parser.add_argument('--type', choices=('aab', 'ipa'), required=True)
    parser.add_argument('path')
    arguments = parser.parse_args()
    try:
        signatures = inspect(arguments.path, arguments.type)
    except Usage as error:
        print(f'archive.py: {error}', file=sys.stderr)
        sys.exit(arguments.usage)
    except Rejected as error:
        print(f'archive.py: {error}', file=sys.stderr)
        sys.exit(arguments.malformed)
    except Exception as error:
        print(f'archive.py: not a readable zip archive: {type(error).__name__}: {error}', file=sys.stderr)
        sys.exit(arguments.malformed)
    print(signatures)


if __name__ == '__main__':
    main()
