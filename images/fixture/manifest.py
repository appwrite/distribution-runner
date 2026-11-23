import argparse
import json
import sys
import zipfile
from collections.abc import Callable
from typing import NamedTuple

MANIFEST_ENTRY = 'base/manifest/AndroidManifest.xml'
ANDROID_NAMESPACE = 'http://schemas.android.com/apk/res/android'
SIGNATURE_EXTENSIONS = ('SF', 'RSA', 'DSA', 'EC')
VERSION_CODE_MAX = 2_100_000_000
VERSION_NAME_MAX = 255

WIRE_VARINT = 0
WIRE_FIXED64 = 1
WIRE_LENGTH = 2
WIRE_FIXED32 = 5

NODE_ELEMENT = 1
ELEMENT_NAME = 3
ELEMENT_ATTRIBUTE = 4
ATTRIBUTE_NAMESPACE = 1
ATTRIBUTE_NAME = 2
ATTRIBUTE_VALUE = 3
ATTRIBUTE_COMPILED_ITEM = 6
ITEM_STRING = 2
ITEM_PRIMITIVE = 7
STRING_VALUE = 1
PRIMITIVE_INT_DECIMAL = 6

EXIT_UNEXPECTED = 1
EXIT_INVALID = 2
EXIT_SIGNED = 3


class Failure(Exception):
    def __init__(self, message: str, code: int):
        super().__init__(message)
        self.code = code


class Field(NamedTuple):
    number: int
    wire: int
    value: int | bytes
    raw: bytes | None


Edit = Callable[[list[Field]], list[Field]]


def read_varint(data: bytes, position: int) -> tuple[int, int]:
    result = 0
    shift = 0
    while True:
        if position >= len(data):
            raise Failure('truncated varint', EXIT_UNEXPECTED)
        byte = data[position]
        position += 1
        result |= (byte & 0x7F) << shift
        if byte < 0x80:
            return result, position
        shift += 7
        if shift > 63:
            raise Failure('varint too long', EXIT_UNEXPECTED)


def write_varint(value: int) -> bytes:
    if value < 0:
        value += 1 << 64
    out = bytearray()
    while value >= 0x80:
        out.append((value & 0x7F) | 0x80)
        value >>= 7
    out.append(value)
    return bytes(out)


def decode(data: bytes) -> list[Field]:
    fields = []
    position = 0
    while position < len(data):
        start = position
        key, position = read_varint(data, position)
        number, wire = key >> 3, key & 0x07
        if wire == WIRE_VARINT:
            value, position = read_varint(data, position)
        elif wire == WIRE_LENGTH:
            length, position = read_varint(data, position)
            if position + length > len(data):
                raise Failure('truncated length-delimited field', EXIT_UNEXPECTED)
            value = data[position:position + length]
            position += length
        elif wire in (WIRE_FIXED64, WIRE_FIXED32):
            size = 8 if wire == WIRE_FIXED64 else 4
            if position + size > len(data):
                raise Failure('truncated fixed-width field', EXIT_UNEXPECTED)
            value = data[position:position + size]
            position += size
        else:
            raise Failure(f'unsupported wire type {wire}', EXIT_UNEXPECTED)
        fields.append(Field(number, wire, value, data[start:position]))
    return fields


def encode(fields: list[Field]) -> bytes:
    out = bytearray()
    for field in fields:
        if field.raw is not None:
            out += field.raw
            continue
        out += write_varint(field.number << 3 | field.wire)
        if field.wire == WIRE_VARINT:
            out += write_varint(field.value)
        elif field.wire == WIRE_LENGTH:
            out += write_varint(len(field.value)) + field.value
        else:
            out += field.value
    return bytes(out)


def first(fields: list[Field], number: int) -> Field | None:
    return next((field for field in fields if field.number == number), None)


def text(fields: list[Field], number: int) -> str:
    field = first(fields, number)
    return field.value.decode('utf-8') if field is not None else ''


def replace(fields: list[Field], number: int, wire: int, value: int | bytes) -> list[Field]:
    updated = Field(number, wire, value, None)
    if first(fields, number) is None:
        return [*fields, updated]
    return [updated if field.number == number else field for field in fields]


def replace_message(fields: list[Field], number: int, edit: Edit) -> list[Field]:
    field = first(fields, number)
    if field is None:
        raise Failure(f'missing message field {number}', EXIT_UNEXPECTED)
    return replace(fields, number, WIRE_LENGTH, encode(edit(decode(field.value))))


def root_element(manifest: bytes) -> list[Field]:
    element = first(decode(manifest), NODE_ELEMENT)
    if element is None or element.wire != WIRE_LENGTH:
        raise Failure('manifest has no root element', EXIT_UNEXPECTED)
    fields = decode(element.value)
    if text(fields, ELEMENT_NAME) != 'manifest':
        raise Failure('root element is not <manifest>', EXIT_UNEXPECTED)
    return fields


def attribute_key(fields: list[Field]) -> tuple[str, str]:
    return text(fields, ATTRIBUTE_NAMESPACE), text(fields, ATTRIBUTE_NAME)


def attributes(manifest: bytes) -> dict[tuple[str, str], list[Field]]:
    found = {}
    for field in root_element(manifest):
        if field.number == ELEMENT_ATTRIBUTE and field.wire == WIRE_LENGTH:
            attribute = decode(field.value)
            found[attribute_key(attribute)] = attribute
    return found


def set_package(attribute: list[Field], identifier: str) -> list[Field]:
    return replace(attribute, ATTRIBUTE_VALUE, WIRE_LENGTH, identifier.encode('utf-8'))


def set_version_code(attribute: list[Field], code: int) -> list[Field]:
    attribute = replace(attribute, ATTRIBUTE_VALUE, WIRE_LENGTH, str(code).encode('utf-8'))
    return replace_message(
        attribute,
        ATTRIBUTE_COMPILED_ITEM,
        lambda item: replace_message(
            item,
            ITEM_PRIMITIVE,
            lambda primitive: replace(primitive, PRIMITIVE_INT_DECIMAL, WIRE_VARINT, code),
        ),
    )


def set_version_name(attribute: list[Field], name: str) -> list[Field]:
    encoded = name.encode('utf-8')
    attribute = replace(attribute, ATTRIBUTE_VALUE, WIRE_LENGTH, encoded)
    item = first(attribute, ATTRIBUTE_COMPILED_ITEM)
    if item is None or first(decode(item.value), ITEM_STRING) is None:
        return attribute
    return replace_message(
        attribute,
        ATTRIBUTE_COMPILED_ITEM,
        lambda fields: replace_message(
            fields,
            ITEM_STRING,
            lambda string: replace(string, STRING_VALUE, WIRE_LENGTH, encoded),
        ),
    )


def rewrite_manifest(manifest: bytes, edits: dict[tuple[str, str], Edit]) -> bytes:
    pending = dict(edits)

    def edit_element(element: list[Field]) -> list[Field]:
        rewritten = []
        for field in element:
            if field.number == ELEMENT_ATTRIBUTE and field.wire == WIRE_LENGTH:
                attribute = decode(field.value)
                change = pending.pop(attribute_key(attribute), None)
                if change is not None:
                    field = Field(field.number, field.wire, encode(change(attribute)), None)
            rewritten.append(field)
        return rewritten

    root_element(manifest)
    rewritten = encode(replace_message(decode(manifest), NODE_ELEMENT, edit_element))
    if pending:
        missing = ', '.join(name for _, name in pending)
        raise Failure(f'manifest has no attribute {missing}', EXIT_UNEXPECTED)
    return rewritten


def identifier_error(value: str) -> str | None:
    segments = value.split('.')
    if len(segments) < 2:
        return 'needs at least two dot-separated segments'
    for segment in segments:
        if not segment:
            return 'has an empty segment'
        if not (segment[0].isascii() and segment[0].isalpha()):
            return f'segment {segment!r} must start with a letter'
        if not all(character.isascii() and (character.isalnum() or character == '_') for character in segment):
            return f'segment {segment!r} may contain only letters, digits and underscores'
    return None


def version_code_value(value: str) -> int:
    if not (value.isascii() and value.isdigit()):
        raise Failure(f'invalid version code {value!r}: must be an integer', EXIT_INVALID)
    code = int(value)
    if not 1 <= code <= VERSION_CODE_MAX:
        raise Failure(f'invalid version code {value!r}: must be between 1 and {VERSION_CODE_MAX}', EXIT_INVALID)
    return code


def version_name_value(value: str) -> str:
    if len(value) > VERSION_NAME_MAX or not value.isprintable():
        raise Failure(f'invalid version name {value!r}: must be 1 to {VERSION_NAME_MAX} printable characters', EXIT_INVALID)
    return value


def identifier_value(value: str) -> str:
    error = identifier_error(value)
    if error is not None:
        raise Failure(f'invalid identifier {value!r}: {error}', EXIT_INVALID)
    return value


def is_signature(name: str) -> bool:
    if not name.startswith('META-INF/'):
        return False
    file = name[len('META-INF/'):]
    if '/' in file or '.' not in file:
        return False
    return file.rsplit('.', 1)[1] in SIGNATURE_EXTENSIONS


def assert_unsigned(bundle: zipfile.ZipFile) -> None:
    signatures = [name for name in bundle.namelist() if is_signature(name)]
    if signatures:
        raise Failure(f'bundle is signed: {", ".join(signatures)}', EXIT_SIGNED)


def rewrite(arguments: argparse.Namespace) -> None:
    edits: dict[tuple[str, str], Edit] = {}
    if arguments.identifier:
        identifier = identifier_value(arguments.identifier)
        edits[('', 'package')] = lambda attribute: set_package(attribute, identifier)
    if arguments.version_name:
        name = version_name_value(arguments.version_name)
        edits[(ANDROID_NAMESPACE, 'versionName')] = lambda attribute: set_version_name(attribute, name)
    if arguments.version_code:
        code = version_code_value(arguments.version_code)
        edits[(ANDROID_NAMESPACE, 'versionCode')] = lambda attribute: set_version_code(attribute, code)

    with zipfile.ZipFile(arguments.input) as source:
        assert_unsigned(source)
        entries = source.infolist()
        if MANIFEST_ENTRY not in source.namelist():
            raise Failure(f'bundle has no {MANIFEST_ENTRY}', EXIT_UNEXPECTED)
        with zipfile.ZipFile(arguments.output, 'w') as target:
            for entry in entries:
                data = source.read(entry)
                if entry.filename == MANIFEST_ENTRY:
                    data = rewrite_manifest(data, edits)
                clone = zipfile.ZipInfo(entry.filename, date_time=entry.date_time)
                clone.compress_type = entry.compress_type
                clone.create_system = entry.create_system
                clone.external_attr = entry.external_attr
                target.writestr(clone, data)


def result(arguments: argparse.Namespace) -> None:
    with zipfile.ZipFile(arguments.bundle) as bundle:
        assert_unsigned(bundle)
        found = attributes(bundle.read(MANIFEST_ENTRY))

    def value(key: tuple[str, str]) -> str:
        if key not in found:
            raise Failure(f'manifest has no attribute {key[1]}', EXIT_UNEXPECTED)
        return text(found[key], ATTRIBUTE_VALUE)

    with open(arguments.template, encoding='utf-8') as template:
        document = json.load(template)
    document['identifier'] = value(('', 'package'))
    document['versionName'] = value((ANDROID_NAMESPACE, 'versionName'))
    document['versionCode'] = int(value((ANDROID_NAMESPACE, 'versionCode')))
    document['toolchain']['image'] = arguments.image
    json.dump(document, sys.stdout)
    sys.stdout.write('\n')


def parser() -> argparse.ArgumentParser:
    root = argparse.ArgumentParser(description='Rewrite and report the identity of the fixture bundle.')
    commands = root.add_subparsers(dest='command', required=True)

    rewriter = commands.add_parser('rewrite', help='write a copy of the bundle with a new identity; an empty value keeps the current one')
    rewriter.add_argument('--identifier', default='')
    rewriter.add_argument('--version-name', default='')
    rewriter.add_argument('--version-code', default='')
    rewriter.add_argument('input')
    rewriter.add_argument('output')
    rewriter.set_defaults(handler=rewrite)

    reporter = commands.add_parser('result', help='print result.json for a bundle')
    reporter.add_argument('--template', required=True)
    reporter.add_argument('--image', required=True)
    reporter.add_argument('bundle')
    reporter.set_defaults(handler=result)

    return root


def main() -> int:
    arguments = parser().parse_args()
    try:
        arguments.handler(arguments)
    except Failure as failure:
        print(f'manifest.py: {failure}', file=sys.stderr)
        return failure.code
    except (OSError, zipfile.BadZipFile, KeyError, ValueError, UnicodeDecodeError) as error:
        print(f'manifest.py: {error}', file=sys.stderr)
        return EXIT_UNEXPECTED
    return 0


if __name__ == '__main__':
    sys.exit(main())
