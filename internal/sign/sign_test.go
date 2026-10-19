package sign

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// testKey mirrors the production key layout: a primary key able to sign and certify,
// an encryption subkey, and a separate signing subkey that makes all signatures.
type testKey struct {
	entity  *openpgp.Entity
	secret  []byte // binary secret export, like `gpg --export-secret-keys`
	signFpr string
	encFpr  string
	primary string
}

func newTestKey(t *testing.T) testKey {
	t.Helper()
	cfg := &packet.Config{Algorithm: packet.PubKeyAlgoRSA, RSABits: 2048}
	e, err := openpgp.NewEntity("Mock ClickHouse Repositories Key", "", "packages@example.com", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.AddSigningSubkey(cfg); err != nil {
		t.Fatal(err)
	}
	var secret bytes.Buffer
	if err := e.SerializePrivate(&secret, nil); err != nil {
		t.Fatal(err)
	}
	k := testKey{entity: e, secret: secret.Bytes(), primary: fmt.Sprintf("%X", e.PrimaryKey.Fingerprint)}
	for _, sub := range e.Subkeys {
		switch {
		case sub.Sig.FlagSign:
			k.signFpr = fmt.Sprintf("%X", sub.PublicKey.Fingerprint)
		case sub.Sig.FlagEncryptCommunications:
			k.encFpr = fmt.Sprintf("%X", sub.PublicKey.Fingerprint)
		}
	}
	if k.signFpr == "" || k.encFpr == "" {
		t.Fatal("test key misses a subkey")
	}
	return k
}

func armored(t *testing.T, blockType string, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := armor.Encode(&out, blockType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestLoadFormats(t *testing.T) {
	k := newTestKey(t)
	b64 := base64.StdEncoding.EncodeToString(k.secret)
	var wrapped strings.Builder
	for i := 0; i < len(b64); i += 76 {
		wrapped.WriteString(b64[i:min(i+76, len(b64))] + "\n")
	}
	formats := map[string][]byte{
		"binary":         k.secret,
		"base64":         []byte(b64),
		"base64 wrapped": []byte(wrapped.String()),
		"armored":        armored(t, openpgp.PrivateKeyType, k.secret),
	}
	for name, data := range formats {
		t.Run(name, func(t *testing.T) {
			s, err := Load(data, k.signFpr, nil)
			if err != nil {
				t.Fatal(err)
			}
			if s.Fingerprint() != k.signFpr || s.PrimaryFingerprint() != k.primary {
				t.Fatalf("selected %s of %s, want %s of %s", s.Fingerprint(), s.PrimaryFingerprint(), k.signFpr, k.primary)
			}
		})
	}
}

func TestFingerprintSpellings(t *testing.T) {
	k := newTestKey(t)
	spaced := strings.Join(strings.SplitAfterN(k.signFpr, "", 40), "")
	for _, fpr := range []string{k.signFpr, strings.ToLower(k.signFpr), "0x" + k.signFpr, k.signFpr + "!", spaced[:20] + " " + spaced[20:]} {
		if _, err := Load(k.secret, fpr, nil); err != nil {
			t.Errorf("%q: %v", fpr, err)
		}
	}
}

func TestLoadRejects(t *testing.T) {
	k := newTestKey(t)
	cases := map[string]string{
		"encryption subkey": k.encFpr,
		"unknown key":       strings.Repeat("A", 40),
		"short key id":      k.signFpr[24:],
		"not hex":           strings.Repeat("Z", 40),
	}
	for name, fpr := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(k.secret, fpr, nil); err == nil {
				t.Fatalf("Load(%s) succeeded", fpr)
			}
		})
	}
	t.Run("public key only", func(t *testing.T) {
		var pub bytes.Buffer
		if err := k.entity.Serialize(&pub); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(pub.Bytes(), k.signFpr, nil); err == nil {
			t.Fatal("Load of a public key succeeded")
		}
	})
}

func TestSignaturesComeFromTheNamedKey(t *testing.T) {
	k := newTestKey(t)
	s, err := Load(k.secret, k.signFpr, nil)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("Origin: ClickHouse\nLabel: ClickHouse\nCodename: stable\n")

	det, err := s.DetachSign(msg)
	if err != nil {
		t.Fatal(err)
	}
	assertSignature(t, det, s.key.PublicKey.KeyId)

	cs, err := s.Clearsign(msg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(cs, []byte("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA512\n")) {
		t.Fatalf("unexpected clearsign header:\n%s", cs)
	}
	sigStart := bytes.Index(cs, []byte("-----BEGIN PGP SIGNATURE-----"))
	assertSignature(t, cs[sigStart:], s.key.PublicKey.KeyId)

	v := s.Verifier()
	if err := v.VerifyDetached(msg, det); err != nil {
		t.Fatalf("detached: %v", err)
	}
	plain, err := v.VerifyClearsigned(cs)
	if err != nil {
		t.Fatalf("clearsigned: %v", err)
	}
	if !bytes.Equal(bytes.TrimSuffix(plain, []byte("\n")), bytes.TrimSuffix(msg, []byte("\n"))) {
		t.Fatalf("clearsigned plaintext %q, want %q", plain, msg)
	}
	if err := v.VerifyDetached(append(msg, 'x'), det); err == nil {
		t.Fatal("tampered message verified")
	}
}

func TestPublicKeyRoundTrip(t *testing.T) {
	k := newTestKey(t)
	s, err := Load(k.secret, k.signFpr, nil)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := s.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(pub, []byte("PRIVATE")) {
		t.Fatal("public key export contains a private key block")
	}
	v, err := LoadPublic(pub)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("repomd")
	sig, err := s.DetachSign(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.VerifyDetached(msg, sig); err != nil {
		t.Fatal(err)
	}
	for _, fpr := range []string{k.signFpr, k.primary, k.encFpr} {
		v, err := LoadPublicFor(pub, fpr)
		if err != nil {
			t.Fatalf("%s: %v", fpr, err)
		}
		if err := v.VerifyDetached(msg, sig); err != nil {
			t.Fatal(err)
		}
	}
	other := newTestKey(t)
	if _, err := LoadPublicFor(pub, other.signFpr); err == nil {
		t.Fatal("another fingerprint accepted")
	}
	var two bytes.Buffer
	if err := k.entity.Serialize(&two); err != nil {
		t.Fatal(err)
	}
	if err := other.entity.Serialize(&two); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPublicFor(two.Bytes(), k.signFpr); err == nil {
		t.Fatal("an export with two keys accepted")
	}
}

func TestPassphrase(t *testing.T) {
	k := newTestKey(t)
	pass := []byte("secret")
	for _, key := range append([]*packet.PrivateKey{k.entity.PrivateKey}, subkeyPrivates(k.entity)...) {
		if err := key.Encrypt(pass); err != nil {
			t.Fatal(err)
		}
	}
	var enc bytes.Buffer
	if err := k.entity.SerializePrivateWithoutSigning(&enc, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(enc.Bytes(), k.signFpr, nil); err == nil {
		t.Fatal("encrypted key loaded without a passphrase")
	}
	if _, err := Load(enc.Bytes(), k.signFpr, []byte("wrong")); err == nil {
		t.Fatal("encrypted key loaded with a wrong passphrase")
	}
	s, err := Load(enc.Bytes(), k.signFpr, pass)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DetachSign([]byte("x")); err != nil {
		t.Fatal(err)
	}
}

// TestGPGVerifies checks our signatures with gpgv, independent of go-crypto.
func TestGPGVerifies(t *testing.T) {
	k := newTestKey(t)
	s, err := Load(k.secret, k.signFpr, nil)
	if err != nil {
		t.Fatal(err)
	}
	gpgvVerify(t, s)
}

// TestMockKey runs the gpgv check with a key exported by gpg itself:
// CH_DEPLOY_TEST_SIGNING_KEY_FILE (base64 secret export) and CH_DEPLOY_TEST_SIGN_WITH.
func TestMockKey(t *testing.T) {
	file, fpr := os.Getenv("CH_DEPLOY_TEST_SIGNING_KEY_FILE"), os.Getenv("CH_DEPLOY_TEST_SIGN_WITH")
	if file == "" || fpr == "" {
		t.Skip("CH_DEPLOY_TEST_SIGNING_KEY_FILE and CH_DEPLOY_TEST_SIGN_WITH are not set")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Load(data, fpr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.PrimaryFingerprint() == s.Fingerprint() {
		t.Fatal("mock key signs with its primary key, expected a subkey")
	}
	gpgvVerify(t, s)
}

func gpgvVerify(t *testing.T, s *Signer) {
	t.Helper()
	gpgv, err := exec.LookPath("gpgv")
	if err != nil {
		t.Skip("gpgv is not installed")
	}
	dir := t.TempDir()
	var keyring bytes.Buffer
	if err := s.entity.Serialize(&keyring); err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ring := write("keyring.gpg", keyring.Bytes())
	msg := []byte("Origin: ClickHouse\nDate: Tue, 29 Sep 2026 14:18:48 UTC\n SHA256:\n 8d92 2845037 main/binary-amd64/Packages\n- dash line\n")
	release := write("Release", msg)
	det, err := s.DetachSign(msg)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := s.Clearsign(msg)
	if err != nil {
		t.Fatal(err)
	}

	for name, args := range map[string][]string{
		"Release.gpg": {write("Release.gpg", det), release},
		"InRelease":   {write("InRelease", cs)},
	} {
		out, err := exec.Command(gpgv, append([]string{"--keyring", ring}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("gpgv %s: %v\n%s", name, err, out)
		}
		if !strings.Contains(string(out), s.Fingerprint()) && !strings.Contains(string(out), s.Fingerprint()[24:]) {
			t.Fatalf("gpgv %s: signature not made by %s:\n%s", name, s.Fingerprint(), out)
		}
	}
}

func assertSignature(t *testing.T, armoredSig []byte, keyID uint64) {
	t.Helper()
	block, err := armor.Decode(bytes.NewReader(armoredSig))
	if err != nil {
		t.Fatal(err)
	}
	p, err := packet.Read(block.Body)
	if err != nil {
		t.Fatal(err)
	}
	sig, ok := p.(*packet.Signature)
	if !ok {
		t.Fatalf("got %T, want a signature packet", p)
	}
	if sig.IssuerKeyId == nil || *sig.IssuerKeyId != keyID {
		t.Fatalf("signature issuer %v, want %X", sig.IssuerKeyId, keyID)
	}
	if sig.Hash != Hash {
		t.Fatalf("signature hash %v, want %v", sig.Hash, Hash)
	}
	if len(sig.Notations) != 0 {
		t.Fatalf("signature carries notations %v, gpg adds none", sig.Notations)
	}
	if !bytes.HasSuffix(armoredSig, []byte("-----END PGP SIGNATURE-----\n")) {
		t.Fatal("armored signature does not end with a newline")
	}
}

func subkeyPrivates(e *openpgp.Entity) []*packet.PrivateKey {
	var keys []*packet.PrivateKey
	for _, sub := range e.Subkeys {
		keys = append(keys, sub.PrivateKey)
	}
	return keys
}
