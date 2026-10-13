// Package hashes computes every hash the repositories need in one pass.
//
// MD5 and SHA1 are required by APT indexes and S3 Content-MD5; nothing relies on them for security.
package hashes

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
)

// Sums are the size and the hex hashes of some content.
type Sums struct {
	Size   int64  `json:"size"`
	MD5    string `json:"md5"`
	SHA1   string `json:"sha1"`
	SHA256 string `json:"sha256"`
	SHA512 string `json:"sha512"`
}

// Sum reads r to the end.
func Sum(r io.Reader) (Sums, error) {
	m, s1, s256, s512 := md5.New(), sha1.New(), sha256.New(), sha512.New()
	n, err := io.Copy(io.MultiWriter(m, s1, s256, s512), r)
	if err != nil {
		return Sums{}, err
	}
	enc := func(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }
	return Sums{Size: n, MD5: enc(m), SHA1: enc(s1), SHA256: enc(s256), SHA512: enc(s512)}, nil
}

// Bytes hashes b.
func Bytes(b []byte) Sums {
	s, _ := Sum(bytes.NewReader(b)) // reading a bytes.Reader into hashes cannot fail
	return s
}

// ContentMD5 returns the base64 MD5 of r, as the S3 Content-MD5 header wants it.
func ContentMD5(r io.Reader) (string, error) {
	h := md5.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

// Base64 converts a hex hash to the base64 form of S3 checksum headers.
func Base64(hexSum string) (string, error) {
	b, err := hex.DecodeString(hexSum)
	if err != nil {
		return "", fmt.Errorf("hash %q: %w", hexSum, err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// File hashes the file at path.
func File(path string) (Sums, error) {
	f, err := os.Open(path)
	if err != nil {
		return Sums{}, err
	}
	defer f.Close()
	s, err := Sum(f)
	if err != nil {
		return Sums{}, fmt.Errorf("hash %s: %w", path, err)
	}
	return s, nil
}
