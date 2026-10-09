package apt

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
)

// loadKeyring reads an OpenPGP keyring file, binary (.gpg) or ASCII-armored (.asc).
func loadKeyring(path string) (openpgp.EntityList, error) {
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

// openInRelease decodes a clearsigned InRelease and returns its cleartext. If
// kr is non-nil the signature is verified and a failure is an error.
func openInRelease(data []byte, kr openpgp.EntityList) ([]byte, error) {
	block, _ := clearsign.Decode(data)
	if block == nil {
		return nil, errors.New("InRelease is not a clearsigned message")
	}
	if kr != nil {
		if _, err := openpgp.CheckDetachedSignature(kr, bytes.NewReader(block.Bytes), block.ArmoredSignature.Body, nil); err != nil {
			return nil, fmt.Errorf("InRelease signature verification failed: %w", err)
		}
	}
	return block.Plaintext, nil
}

// checkDetached verifies Release.gpg over Release.
func checkDetached(kr openpgp.EntityList, signed, sig []byte) error {
	var err error
	if _, err = openpgp.CheckDetachedSignature(kr, bytes.NewReader(signed), bytes.NewReader(sig), nil); err == nil {
		return nil
	}
	// Release.gpg is usually ASCII-armored, but may be binary.
	if _, aerr := openpgp.CheckArmoredDetachedSignature(kr, bytes.NewReader(signed), bytes.NewReader(sig), nil); aerr == nil {
		return nil
	}
	return fmt.Errorf("release.gpg signature verification failed: %w", err)
}
