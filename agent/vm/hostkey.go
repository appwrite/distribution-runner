package vm

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
)

const hostKeyType = "ssh-ed25519"

// hostKey is an ed25519 ssh host key minted for one machine, so no two machines share the key the agent pins.
type hostKey struct {
	// Private is the OpenSSH private key file sshd reads.
	Private []byte
	// Public is the "ssh-ed25519 AAAA…" form a known_hosts line and a .pub file carry.
	Public string
}

// newHostKey mints a host key in the openssh-key-v1 format (OpenSSH PROTOCOL.key), unencrypted, as ssh-keygen
// writes one.
func newHostKey(comment string) (hostKey, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return hostKey{}, err
	}
	blob := sshString(sshString(nil, []byte(hostKeyType)), public)

	var check [4]byte
	if _, err := rand.Read(check[:]); err != nil {
		return hostKey{}, err
	}
	secret := append(check[:], check[:]...)
	secret = sshString(secret, []byte(hostKeyType))
	secret = sshString(secret, public)
	secret = sshString(secret, private)
	secret = sshString(secret, []byte(comment))
	for padding := byte(1); len(secret)%8 != 0; padding++ {
		secret = append(secret, padding)
	}

	data := append([]byte("openssh-key-v1"), 0)
	data = sshString(data, []byte("none"))
	data = sshString(data, []byte("none"))
	data = sshString(data, nil)
	data = binary.BigEndian.AppendUint32(data, 1)
	data = sshString(data, blob)
	data = sshString(data, secret)
	return hostKey{
		Private: pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: data}),
		Public:  hostKeyType + " " + base64.StdEncoding.EncodeToString(blob),
	}, nil
}

func sshString(buffer []byte, value []byte) []byte {
	buffer = binary.BigEndian.AppendUint32(buffer, uint32(len(value)))
	return append(buffer, value...)
}
