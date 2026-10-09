package apt

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
)

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
