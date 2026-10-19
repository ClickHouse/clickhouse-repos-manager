// Package signtest gives tests a throwaway signing key with the structure of
// the production one: a primary key and a signing subkey.
package signtest

import (
	"bytes"
	"fmt"
	"sync"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/sign"
)

// Key is a test signing key.
type Key struct {
	// Secret is the binary secret export, as in SSM.
	Secret []byte
	// SignWith is the fingerprint of the signing subkey.
	SignWith string
	Signer   *sign.Signer
	Public   []byte
}

var (
	keys  [2]Key
	errs  [2]error
	onces [2]sync.Once
)

// New returns the test key; it is generated once per test binary.
func New(t testing.TB) Key { return get(t, 0) }

// Other returns a second test key, different from New.
func Other(t testing.TB) Key { return get(t, 1) }

func get(t testing.TB, i int) Key {
	t.Helper()
	onces[i].Do(func() { keys[i], errs[i] = generate(fmt.Sprintf("Test Key %d", i)) })
	if errs[i] != nil {
		t.Fatal(errs[i])
	}
	return keys[i]
}

func generate(name string) (Key, error) {
	cfg := &packet.Config{Algorithm: packet.PubKeyAlgoRSA, RSABits: 2048}
	e, err := openpgp.NewEntity(name, "", "packages@example.com", cfg)
	if err != nil {
		return Key{}, err
	}
	if err := e.AddSigningSubkey(cfg); err != nil {
		return Key{}, err
	}
	var secret bytes.Buffer
	if err := e.SerializePrivate(&secret, nil); err != nil {
		return Key{}, err
	}
	var fpr string
	for _, sub := range e.Subkeys {
		if sub.Sig.FlagSign {
			fpr = fmt.Sprintf("%X", sub.PublicKey.Fingerprint)
		}
	}
	s, err := sign.Load(secret.Bytes(), fpr, nil)
	if err != nil {
		return Key{}, err
	}
	pub, err := s.PublicKey()
	if err != nil {
		return Key{}, err
	}
	return Key{Secret: secret.Bytes(), SignWith: fpr, Signer: s, Public: pub}, nil
}
