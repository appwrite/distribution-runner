#!/usr/bin/env python3
import base64
import binascii
import json
import os
import sys

KEYS = ('keystore', 'storePassword', 'keyAlias', 'keyPassword')
EXIT_USAGE = 7


def refuse(reason):
    print(f'bundle.py: signing bundle {reason}', file=sys.stderr)
    sys.exit(EXIT_USAGE)


def consume(path):
    try:
        with open(path, 'rb') as handle:
            return handle.read()
    except OSError:
        return None
    finally:
        try:
            os.remove(path)
        except OSError:
            pass


def parse(raw):
    try:
        document = json.loads(raw)
    except ValueError:
        refuse('is not valid JSON')
    if not isinstance(document, dict) or sorted(document) != sorted(KEYS):
        refuse('must have exactly the keys ' + ', '.join(KEYS))
    for key in KEYS:
        if not isinstance(document[key], str) or not document[key]:
            refuse(f'{key} must be a non-empty string')
    for key in ('storePassword', 'keyPassword', 'keyAlias'):
        if any(character in document[key] for character in '\r\n\0'):
            refuse(f'{key} must be a single line')
    if document['keyAlias'].startswith('-'):
        refuse('keyAlias must not start with a hyphen')
    try:
        keystore = base64.b64decode(document['keystore'], validate=True)
    except binascii.Error:
        refuse('keystore is not valid base64')
    if not keystore:
        refuse('keystore is empty')
    return {
        'keystore': keystore,
        'store-password': document['storePassword'].encode(),
        'key-password': document['keyPassword'].encode(),
        'alias': document['keyAlias'].encode(),
    }


def main(path, directory):
    raw = consume(path)
    if raw is None:
        refuse(f'{path} is missing')
    for name, content in parse(raw).items():
        descriptor = os.open(os.path.join(directory, name), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, 'wb') as handle:
            handle.write(content)


if __name__ == '__main__':
    try:
        main(*sys.argv[1:])
    except SystemExit:
        raise
    except Exception:
        refuse('could not be read')
