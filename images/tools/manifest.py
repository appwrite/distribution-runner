#!/usr/bin/env python3
import argparse
import json
import sys
import xml.etree.ElementTree as ElementTree

ANDROID = '{http://schemas.android.com/apk/res/android}'


class Refused(Exception):
    pass


def integer(name, value, required):
    if value is None and not required:
        return None
    if value is None or not (value.isascii() and value.isdigit()):
        raise Refused(f'{name} is {value!r}, not an integer')
    return int(value)


def identity(path):
    with open(path, 'rb') as handle:
        document = handle.read()
    if b'<!DOCTYPE' in document or b'<!ENTITY' in document:
        raise Refused('manifest XML declares a document type')
    root = ElementTree.fromstring(document)
    if root.tag != 'manifest':
        raise Refused(f'root element is <{root.tag}>, expected <manifest>')
    sdk = root.find('uses-sdk')
    sdk = sdk.attrib if sdk is not None else {}
    return {
        'identifier': root.get('package') or None,
        'versionName': root.get(ANDROID + 'versionName') or None,
        'versionCode': integer('versionCode', root.get(ANDROID + 'versionCode'), True),
        'minSdk': integer('minSdk', sdk.get(ANDROID + 'minSdkVersion'), False),
        'targetSdk': integer('targetSdk', sdk.get(ANDROID + 'targetSdkVersion'), False),
    }


def main():
    parser = argparse.ArgumentParser(description='Read the identity of a bundle from the XML that bundletool dump manifest prints.')
    parser.add_argument('--exit', type=int, required=True, help='exit code for an unreadable manifest or a mismatch')
    parser.add_argument('--identifier', help='refuse any other package')
    parser.add_argument('--version-code', type=int, help='refuse any other versionCode')
    parser.add_argument('path')
    arguments = parser.parse_args()
    try:
        found = identity(arguments.path)
        if arguments.identifier is not None and found['identifier'] != arguments.identifier:
            raise Refused(f"package is '{found['identifier'] or ''}', expected '{arguments.identifier}'")
        if arguments.version_code is not None and found['versionCode'] != arguments.version_code:
            raise Refused(f"versionCode is '{found['versionCode']}', expected '{arguments.version_code}'")
    except Refused as error:
        print(f'manifest.py: {error}', file=sys.stderr)
        sys.exit(arguments.exit)
    except Exception as error:
        print(f'manifest.py: cannot read the bundle manifest: {type(error).__name__}: {error}', file=sys.stderr)
        sys.exit(arguments.exit)
    print(json.dumps(found))


if __name__ == '__main__':
    main()
