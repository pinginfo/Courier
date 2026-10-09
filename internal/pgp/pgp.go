// Package pgp holds the OpenPGP helpers shared by the providers.
package pgp

import (
	"bytes"
	"fmt"
	"os"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// LoadKeyring reads an OpenPGP keyring file, binary (.gpg) or ASCII-armored (.asc).
func LoadKeyring(path string) (openpgp.EntityList, error) {
	data, err := os.ReadFile(path) // #nosec G304 G703 -- operator-provided config path
	if err != nil {
		return nil, fmt.Errorf("read gpg_keyring: %w", err)
	}
	kr, err := openpgp.ReadKeyRing(bytes.NewReader(data))
	if err != nil {
		var armErr error
		if kr, armErr = openpgp.ReadArmoredKeyRing(bytes.NewReader(data)); armErr != nil {
			return nil, fmt.Errorf("gpg_keyring %s is neither a binary nor an armored keyring: %w", path, err)
		}
	}
	if len(kr) == 0 {
		return nil, fmt.Errorf("gpg_keyring %s contains no keys", path)
	}
	return kr, nil
}

// CheckDetached verifies a detached signature (armored or binary) over signed.
func CheckDetached(kr openpgp.EntityList, signed, sig []byte) error {
	var err error
	if _, err = openpgp.CheckDetachedSignature(kr, bytes.NewReader(signed), bytes.NewReader(sig), nil); err == nil {
		return nil
	}
	if _, aerr := openpgp.CheckArmoredDetachedSignature(kr, bytes.NewReader(signed), bytes.NewReader(sig), nil); aerr == nil {
		return nil
	}
	return err
}
