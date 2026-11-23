#!/usr/bin/env python3
import argparse
import sys

VERIFIED = 'jar verified.'
UNSIGNED = 'unsigned entries'
SIGNER = '- Signed by '
DIGEST = '    Digest algorithm: '
ALGORITHM = '    Signature algorithm: '
STRONG_DIGESTS = ('SHA-256', 'SHA-384', 'SHA-512')
STRONG_ALGORITHMS = tuple(
    f'SHA{size}with{key}' for size in (256, 384, 512) for key in ('RSA', 'ECDSA', 'DSA')
) + ('RSASSA-PSS', 'Ed25519', 'Ed448')
LOG_TAIL_LINES = 20


class Unverified(Exception):
    pass


def summary(lines):
    signers = 0
    digest = ''
    algorithm = ''
    for line in lines:
        if line.startswith(SIGNER):
            signers += 1
        elif signers and line.startswith(DIGEST):
            digest = line[len(DIGEST):]
        elif signers and line.startswith(ALGORITHM):
            algorithm = line[len(ALGORITHM):].split(',', 1)[0]
    return signers, digest, algorithm


def check(lines, expected_digest, expected_algorithm):
    if VERIFIED not in lines:
        raise Unverified(f'jarsigner did not report {VERIFIED}')
    if any(UNSIGNED in line for line in lines):
        raise Unverified('the bundle contains unsigned entries')
    signers, digest, algorithm = summary(lines)
    if signers != 1:
        raise Unverified(f'expected exactly one signer, found {signers}')
    if digest not in ((expected_digest,) if expected_digest else STRONG_DIGESTS):
        raise Unverified(f"digest algorithm is '{digest}', expected {expected_digest or ' or '.join(STRONG_DIGESTS)}")
    if algorithm not in ((expected_algorithm,) if expected_algorithm else STRONG_ALGORITHMS):
        raise Unverified(f"signature algorithm is '{algorithm}', expected {expected_algorithm or 'a SHA-2 or EdDSA algorithm'}")


def main():
    parser = argparse.ArgumentParser(description='Check the output of jarsigner -verify -verbose -certs.')
    parser.add_argument('--exit', type=int, required=True, help='exit code when the signature does not satisfy the checks')
    parser.add_argument('--digest', help='the only accepted digest algorithm (default: any SHA-2)')
    parser.add_argument('--algorithm', help='the only accepted signature algorithm (default: any SHA-2 or EdDSA)')
    parser.add_argument('log')
    arguments = parser.parse_args()
    with open(arguments.log, encoding='utf-8', errors='replace') as handle:
        lines = handle.read().splitlines()
    try:
        check(lines, arguments.digest, arguments.algorithm)
    except Unverified as error:
        sys.stderr.write(''.join(line + '\n' for line in lines[-LOG_TAIL_LINES:]))
        print(f'signature.py: signature does not verify: {error}', file=sys.stderr)
        sys.exit(arguments.exit)


if __name__ == '__main__':
    main()
