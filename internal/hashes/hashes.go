// Package hashes computes every hash the repositories need in one pass.
//
// MD5 and SHA1 are required by APT indexes and S3 Content-MD5, and CRC64NVME
// is the full object checksum R2 keeps for every object; nothing relies on
// them for security.
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
	"hash/crc64"
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
	// CRC64NVME is the big endian hex of the CRC-64/NVME.
	CRC64NVME string `json:"crc64nvme"`
}

var nvme = crc64.MakeTable(0x9a6c9329ac4bc9b5)

// NewCRC64NVME returns the CRC-64/NVME hash that S3 checksum headers use.
func NewCRC64NVME() hash.Hash64 { return crc64.New(nvme) }

func enc(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }

// Sum reads r to the end.
func Sum(r io.Reader) (Sums, error) {
	m, s1, s256, s512, c := md5.New(), sha1.New(), sha256.New(), sha512.New(), NewCRC64NVME()
	n, err := io.Copy(io.MultiWriter(m, s1, s256, s512, c), r)
	if err != nil {
		return Sums{}, err
	}
	return Sums{Size: n, MD5: enc(m), SHA1: enc(s1), SHA256: enc(s256), SHA512: enc(s512), CRC64NVME: enc(c)}, nil
}

// Part is the MD5 and CRC64NVME of one multipart upload part, in hex.
type Part struct {
	MD5, CRC64NVME string
}

// SumPart reads r to the end.
func SumPart(r io.Reader) (Part, error) {
	m, c := md5.New(), NewCRC64NVME()
	if _, err := io.Copy(io.MultiWriter(m, c), r); err != nil {
		return Part{}, err
	}
	return Part{MD5: enc(m), CRC64NVME: enc(c)}, nil
}

// Bytes hashes b.
func Bytes(b []byte) Sums {
	s, _ := Sum(bytes.NewReader(b)) // reading a bytes.Reader into hashes cannot fail
	return s
}

// Base64 converts a hex hash to the base64 form of S3 checksum headers.
// It accepts any hash length, so it serves Content-MD5 too.
func Base64(hexSum string) (string, error) {
	b, err := hex.DecodeString(hexSum)
	if err != nil {
		return "", fmt.Errorf("hash %q: %w", hexSum, err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// Hex converts the base64 form of S3 checksum headers back to hex.
func Hex(b64 string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("checksum %q: %w", b64, err)
	}
	return hex.EncodeToString(b), nil
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
