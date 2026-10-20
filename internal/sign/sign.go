// Package sign creates and checks the OpenPGP signatures of repository metadata.
package sign

import (
	"bytes"
	"crypto"
	_ "crypto/sha512" // registers SHA512 for crypto.Hash
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// Hash is the digest of every signature, the same as the reprepro and gpg setup produced.
const Hash = crypto.SHA512

// Signer signs with exactly one signing capable key of an OpenPGP secret key export.
type Signer struct {
	entity *openpgp.Entity
	key    openpgp.Key
	config *packet.Config
}

// Load parses a secret key export and selects the key with the given fingerprint.
//
// data may be armored, binary, or base64 of binary.
// The fingerprint must name a signing capable key exactly: there is no fallback to another
// subkey or to the primary key.
func Load(data []byte, fingerprint string, passphrase []byte) (*Signer, error) {
	want, err := normalizeFingerprint(fingerprint)
	if err != nil {
		return nil, err
	}
	entities, err := readKeyRing(data)
	if err != nil {
		return nil, fmt.Errorf("read signing key: %w", err)
	}

	now := time.Now()
	for _, e := range entities {
		keyID, ok := findKeyID(e, want)
		if !ok {
			continue
		}
		key, ok := e.SigningKeyById(now, keyID)
		if !ok || !bytes.Equal(key.PublicKey.Fingerprint, want) {
			return nil, fmt.Errorf("key %X is not a valid signing key (expired, revoked or not signing capable)", want)
		}
		if key.PrivateKey == nil || key.PrivateKey.Dummy() {
			return nil, fmt.Errorf("key %X has no secret part in the export", want)
		}
		if key.PrivateKey.Encrypted {
			if len(passphrase) == 0 {
				return nil, fmt.Errorf("key %X is protected by a passphrase, none given", want)
			}
			if err := key.PrivateKey.Decrypt(passphrase); err != nil {
				return nil, fmt.Errorf("decrypt key %X: %w", want, err)
			}
		}
		salt := false
		return &Signer{
			entity: e,
			key:    key,
			config: &packet.Config{
				DefaultHash:  Hash,
				SigningKeyId: keyID,
				// gpg adds no salt notation; old gpgv and apt versions should see the same packets.
				NonDeterministicSignaturesViaNotation: &salt,
			},
		}, nil
	}
	return nil, fmt.Errorf("key %X not found in the signing key export", want)
}

// Fingerprint of the key that signs.
func (s *Signer) Fingerprint() string {
	return fmt.Sprintf("%X", s.key.PublicKey.Fingerprint)
}

// PrimaryFingerprint of the key that clients trust.
func (s *Signer) PrimaryFingerprint() string {
	return fmt.Sprintf("%X", s.entity.PrimaryKey.Fingerprint)
}

// Clearsign returns msg as a cleartext signed message, the format of InRelease.
func (s *Signer) Clearsign(msg []byte) ([]byte, error) {
	var out bytes.Buffer
	w, err := clearsign.Encode(&out, s.key.PrivateKey, s.config)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(msg); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return withNewline(out.Bytes()), nil
}

// DetachSign returns an armored detached signature of msg, the format of Release.gpg and repomd.xml.asc.
func (s *Signer) DetachSign(msg []byte) ([]byte, error) {
	var out bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&out, s.entity, bytes.NewReader(msg), s.config); err != nil {
		return nil, err
	}
	return withNewline(out.Bytes()), nil
}

// PublicKey returns the armored public key with all subkeys, the content of repomd.xml.key.
func (s *Signer) PublicKey() ([]byte, error) {
	var out bytes.Buffer
	w, err := armor.Encode(&out, openpgp.PublicKeyType, nil)
	if err != nil {
		return nil, err
	}
	if err := s.entity.Serialize(w); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return withNewline(out.Bytes()), nil
}

// Verifier returns a Verifier that trusts the public part of this key.
func (s *Signer) Verifier() *Verifier {
	return &Verifier{keyring: openpgp.EntityList{s.entity}}
}

// Verifier checks signatures made by a set of public keys.
type Verifier struct {
	keyring openpgp.EntityList
}

// LoadPublic parses an armored or binary public key export of any number of keys.
func LoadPublic(data []byte) (*Verifier, error) {
	entities, err := readKeyRing(data)
	if err != nil {
		return nil, fmt.Errorf("read public key: %w", err)
	}
	return &Verifier{keyring: entities}, nil
}

// VerifyClearsigned checks a cleartext signed message and returns the signed
// text and the fingerprint of the key that signed it.
func (v *Verifier) VerifyClearsigned(data []byte) (plain []byte, signer string, err error) {
	block, rest := clearsign.Decode(data)
	if block == nil {
		return nil, "", errors.New("no cleartext signed message found")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, "", errors.New("unexpected data after the signed message")
	}
	sig, e, err := openpgp.VerifyDetachedSignature(v.keyring, bytes.NewReader(block.Bytes), block.ArmoredSignature.Body, &packet.Config{})
	if err != nil {
		return nil, "", err
	}
	return block.Plaintext, signerOf(sig, e), nil
}

// VerifyDetached checks an armored or binary detached signature of msg and
// returns the fingerprint of the key that made it.
func (v *Verifier) VerifyDetached(msg, sig []byte) (signer string, err error) {
	var body io.Reader = bytes.NewReader(sig)
	if bytes.HasPrefix(bytes.TrimSpace(sig), []byte("-----BEGIN PGP")) {
		block, err := armor.Decode(bytes.NewReader(bytes.TrimSpace(sig)))
		if err != nil {
			return "", err
		}
		if block.Type != openpgp.SignatureType {
			return "", fmt.Errorf("armored %s, want %s", block.Type, openpgp.SignatureType)
		}
		body = block.Body
	}
	s, e, err := openpgp.VerifyDetachedSignature(v.keyring, bytes.NewReader(msg), body, &packet.Config{})
	if err != nil {
		return "", err
	}
	return signerOf(s, e), nil
}

// signerOf is the fingerprint of the key of e that made sig.
func signerOf(sig *packet.Signature, e *openpgp.Entity) string {
	if sig.IssuerFingerprint != nil {
		return fmt.Sprintf("%X", sig.IssuerFingerprint)
	}
	if sig.IssuerKeyId != nil {
		for _, sub := range e.Subkeys {
			if sub.PublicKey.KeyId == *sig.IssuerKeyId {
				return fmt.Sprintf("%X", sub.PublicKey.Fingerprint)
			}
		}
	}
	return fmt.Sprintf("%X", e.PrimaryKey.Fingerprint)
}

// withNewline terminates the armor end line: gpgv reads past an unterminated one and fails.
func withNewline(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] != '\n' {
		return append(b, '\n')
	}
	return b
}

func readKeyRing(data []byte) (openpgp.EntityList, error) {
	trimmed := bytes.TrimSpace(data)
	if bytes.HasPrefix(trimmed, []byte("-----BEGIN PGP")) {
		return openpgp.ReadArmoredKeyRing(bytes.NewReader(trimmed))
	}
	if raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(trimmed)), "")); err == nil {
		return openpgp.ReadKeyRing(bytes.NewReader(raw))
	}
	return openpgp.ReadKeyRing(bytes.NewReader(data))
}

func findKeyID(e *openpgp.Entity, fingerprint []byte) (uint64, bool) {
	if bytes.Equal(e.PrimaryKey.Fingerprint, fingerprint) {
		return e.PrimaryKey.KeyId, true
	}
	for _, sub := range e.Subkeys {
		if bytes.Equal(sub.PublicKey.Fingerprint, fingerprint) {
			return sub.PublicKey.KeyId, true
		}
	}
	return 0, false
}

// normalizeFingerprint accepts the gpg spellings: spaces, a 0x prefix and a trailing "!".
func normalizeFingerprint(s string) ([]byte, error) {
	clean := strings.ToUpper(strings.Join(strings.Fields(s), ""))
	clean = strings.TrimSuffix(strings.TrimPrefix(clean, "0X"), "!")
	if len(clean) != 40 && len(clean) != 64 {
		return nil, fmt.Errorf("sign-with %q is not a full key fingerprint", s)
	}
	fp, err := hex.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("sign-with %q is not a full key fingerprint: %w", s, err)
	}
	return fp, nil
}
