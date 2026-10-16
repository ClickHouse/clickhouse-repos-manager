package hashes

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestBytes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Sums
	}{
		{"", Sums{
			Size:      0,
			MD5:       "d41d8cd98f00b204e9800998ecf8427e",
			SHA1:      "da39a3ee5e6b4b0d3255bfef95601890afd80709",
			SHA256:    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			SHA512:    "cf83e1357eefb8bdf1542850d66d8007d620e4050b5715dc83f4a921d36ce9ce47d0d13c5d85f2b0ff8318d2877eec2f63b931bd47417a81a538327af927da3e",
			CRC64NVME: "0000000000000000",
		}},
		// The CRC-64/NVME check value.
		{"123456789", Sums{
			Size:      9,
			MD5:       "25f9e794323b453885f5181f1b624d0b",
			SHA1:      "f7c3bc1d808e04732adf679965ccc34ca7ae3441",
			SHA256:    "15e2b0d3c33891ebb0f1ef609ec419420c20e320ce94c65fbc8c3312448eb225",
			SHA512:    "d9e6762dd1c8eaf6d61b3c6192fc408d4d6d5f1176d0c29169bc24e71c3f274ad27fcd5811b313d681f7e55ec02d73d499c95455b6b5bb503acf574fba8ffe85",
			CRC64NVME: "ae8b14860a799888",
		}},
		{"abc", Sums{
			Size:      3,
			MD5:       "900150983cd24fb0d6963f7d28e17f72",
			SHA1:      "a9993e364706816aba3e25717850c26c9cd0d89d",
			SHA256:    "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
			SHA512:    "ddaf35a193617abacc417349ae20413112e6fa4e89a97ea20a9eeee64b55d39a2192992a274fc1a836ba3c23a3feebbd454d4423643ce80e2a9ac94fa54ca49f",
			CRC64NVME: "05e5cabb3fc1faeb",
		}},
	} {
		if got := Bytes([]byte(tc.in)); got != tc.want {
			t.Errorf("Bytes(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestFileMatchesBytes(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB, more than one io.Copy buffer
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := File(path)
	if err != nil {
		t.Fatal(err)
	}
	// From md5 and shasum -a 1/256/512.
	want := Sums{
		Size:      1 << 20,
		MD5:       "14d785daab3823736c981ca82a750c98",
		SHA1:      "7b961ac18d33b99122ed5d88d1ce62dd19fc8c69",
		SHA256:    "aca1cd027e979588d14b877b7b0cb8585ad9fec599eb45801992ee5382b3760f",
		SHA512:    "a09d147b3e6756219568acbead41d5f55e905d9198c6c5558bd2eb2d9068e56e5fa1167b028e6c56202451648bd20205e2e195298ced118e7c1339fe0fce7ce8",
		CRC64NVME: "1e8ac40a22d0488e",
	}
	if got != want || Bytes(data) != want {
		t.Fatalf("File = %+v, Bytes = %+v, want %+v", got, Bytes(data), want)
	}
	part, err := SumPart(bytes.NewReader(data))
	if err != nil || part != (Part{MD5: want.MD5, CRC64NVME: want.CRC64NVME}) {
		t.Fatalf("SumPart = %+v, %v", part, err)
	}
	if b, _ := Base64(want.CRC64NVME); b != "HorECiLQSI4=" {
		t.Fatalf("Base64 = %s", b)
	}
	if h, err := Hex("HorECiLQSI4="); err != nil || h != want.CRC64NVME {
		t.Fatalf("Hex = %s, %v", h, err)
	}
	if _, err := File(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("File of a missing path succeeded")
	}
}
