package fcm

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/arandu-io/hesape/encryption"
)

// sealer seals the registration tokens at rest and digests them for lookup.
//
// Both keys are derived from the application's key for this module alone, so
// the key that seals sessions or other modules' secrets is never the one that
// seals a token. The sealed text carries the tenant, the device and the purpose
// it was sealed for: a value copied from one row into another opens as an
// error, not as the other row's token.
type sealer struct {
	enc     *encryption.Encrypter
	hashKey []byte
}

func newSealer(appKey []byte) (*sealer, error) {
	enc, err := encryption.NewEncrypter(derive(appKey, "arandu-fcm:device:seal:v1"), encryption.AES256GCM)
	if err != nil {
		return nil, errors.New("fcm: the key cannot seal tokens")
	}
	return &sealer{enc: enc, hashKey: derive(appKey, "arandu-fcm:device:hash:v1")}, nil
}

// derive is the key for one purpose: an HMAC of the purpose under the
// application's key, 32 bytes.
func derive(appKey []byte, purpose string) []byte {
	mac := hmac.New(sha256.New, appKey)
	_, _ = mac.Write([]byte(purpose))
	return mac.Sum(nil)
}

func sealPrefix(tenant, id string) string {
	return tenant + "\x00" + id + "\x00token\x00"
}

// seal encrypts a registration token for one row of one tenant.
func (s *sealer) seal(tenant, id, token string) (string, error) {
	return s.enc.EncryptString(sealPrefix(tenant, id) + token)
}

// errSealedElsewhere is a sealed token opened on a row it was not sealed for.
var errSealedElsewhere = errors.New("fcm: the sealed token belongs to another device")

// errUnsealable is a sealed token this key cannot open: the key changed, or the
// value is not one this module wrote.
var errUnsealable = errors.New("fcm: the sealed token cannot be opened with this key")

// open decrypts a registration token and checks it was sealed for this row.
func (s *sealer) open(tenant, id, sealed string) (string, error) {
	plain, err := s.enc.DecryptString(sealed)
	if err != nil {
		return "", errUnsealable
	}
	prefix := sealPrefix(tenant, id)
	if !strings.HasPrefix(plain, prefix) {
		return "", errSealedElsewhere
	}
	return strings.TrimPrefix(plain, prefix), nil
}

// hash is the keyed digest of a token inside a tenant. The tenant is part of
// the input, so the same token in two tenants digests to two values and the
// digests of one tenant say nothing about another's.
func (s *sealer) hash(tenant, token string) string {
	mac := hmac.New(sha256.New, s.hashKey)
	_, _ = mac.Write([]byte(tenant))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}
