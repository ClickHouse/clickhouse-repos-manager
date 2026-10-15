package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
)

// AssetLine is one line of the assets list.
type AssetLine struct {
	Key    string `json:"key"`
	Size   int64  `json:"size"`
	MD5    string `json:"md5"`
	SHA1   string `json:"sha1"`
	SHA256 string `json:"sha256"`
	SHA512 string `json:"sha512"`
}

func lineOf(key string, s hashes.Sums) AssetLine {
	return AssetLine{Key: key, Size: s.Size, MD5: s.MD5, SHA1: s.SHA1, SHA256: s.SHA256, SHA512: s.SHA512}
}

// ParseAssets decodes an assets list; every line must end with a newline.
func ParseAssets(body []byte) ([]AssetLine, error) {
	if len(body) > 0 && body[len(body)-1] != '\n' {
		return nil, errors.New("assets: the last line is incomplete")
	}
	var lines []AssetLine
	for i, raw := range bytes.SplitAfter(body, []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var l AssetLine
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&l); err != nil {
			return nil, fmt.Errorf("assets line %d: %w", i+1, err)
		}
		if d.More() || l.Key == "" {
			return nil, fmt.Errorf("assets line %d: malformed", i+1)
		}
		lines = append(lines, l)
	}
	return lines, nil
}

func encodeLine(l AssetLine) []byte {
	b, err := json.Marshal(l)
	if err != nil {
		panic(err)
	}
	return append(b, '\n')
}

// readAssets returns the raw list, its lines and its ETag; a missing list is empty.
func (r *Releaser) readAssets(ctx context.Context, name string) ([]byte, []AssetLine, string, error) {
	body, obj, err := r.Bucket.Get(ctx, AssetsKey(name))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, "", nil
	}
	if err != nil {
		return nil, nil, "", err
	}
	lines, err := ParseAssets(body)
	if err != nil {
		return nil, nil, "", err
	}
	return body, lines, obj.ETag, nil
}

// appendAssets adds the lines the list does not hold yet. A key already listed
// with other hashes is a hard fail. The old list stays a byte prefix of the new.
func (r *Releaser) appendAssets(ctx context.Context, name string, lines []AssetLine) error {
	body, old, etag, err := r.readAssets(ctx, name)
	if err != nil {
		return err
	}
	have := make(map[string]AssetLine, len(old))
	for _, l := range old {
		have[l.Key] = l
	}
	next := bytes.Clone(body)
	for _, l := range lines {
		if o, ok := have[l.Key]; ok {
			if o != l {
				return fmt.Errorf("assets: %s is listed with other hashes (sha256 %s, now %s)", l.Key, o.SHA256, l.SHA256)
			}
			continue
		}
		have[l.Key] = l
		next = append(next, encodeLine(l)...)
	}
	if len(next) == len(body) {
		return nil
	}
	key := AssetsKey(name)
	opts := store.PutOptions{ContentType: "application/jsonl"}
	if etag == "" {
		_, err = r.Bucket.PutIfAbsent(ctx, key, next, opts)
	} else {
		_, err = r.Bucket.PutIfMatch(ctx, key, etag, next, opts)
	}
	if err != nil {
		if got, _, gerr := r.Bucket.Get(ctx, key); gerr == nil && bytes.Equal(got, next) {
			return nil
		}
		return fmt.Errorf("assets: %w", err)
	}
	return nil
}

// AssetsTail returns the last n lines of the assets of the release that holds
// the lock, and their total count; nothing when the lock is free.
func (r *Releaser) AssetsTail(ctx context.Context, n int) ([]string, int, error) {
	cur, found, err := r.read(ctx)
	if err != nil || !found || cur.Lock.State == Free {
		return nil, 0, err
	}
	body, _, _, err := r.readAssets(ctx, cur.Lock.Snapshot)
	if err != nil {
		return nil, 0, err
	}
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if len(body) == 0 {
		lines = nil
	}
	return lines[max(0, len(lines)-n):], len(lines), nil
}
