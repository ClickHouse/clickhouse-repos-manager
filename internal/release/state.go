package release

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/config"
)

// Schema is the version of the lock and manifest bodies this build reads and writes.
const Schema = 1

// Keys of the private state.
const (
	LockKey        = config.StatePrefix + "lock"
	ReleasedPrefix = config.StatePrefix + "released/"
	manifestName   = "manifest.json"
)

// SnapshotDir is the key prefix of a snapshot; the live key follows it.
func SnapshotDir(name string) string { return config.SnapshotsPrefix + name + "/" }

// ManifestKey is the key of a snapshot's manifest.
func ManifestKey(name string) string { return SnapshotDir(name) + manifestName }

// ReleasedKey marks a tag as frozen; unlock writes it.
func ReleasedKey(tag string) string { return ReleasedPrefix + tag }

// State is the lock state.
type State string

const (
	Free        State = "free"
	Locked      State = "locked"
	Staging     State = "staging"
	Committing  State = "committing"
	Committed   State = "committed"
	RollingBack State = "rolling_back"
)

func (s State) valid() bool {
	switch s {
	case Free, Locked, Staging, Committing, Committed, RollingBack:
		return true
	}
	return false
}

// Attempt is one run of lock for the release.
type Attempt struct {
	RunID   string `json:"run_id"`
	RunURL  string `json:"run_url"`
	Started string `json:"started"`
	Version string `json:"version"`
}

// Lock is the body of LockKey.
type Lock struct {
	Schema   int       `json:"schema"`
	Tag      string    `json:"tag"`
	TagDate  string    `json:"tag_date"`
	Snapshot string    `json:"snapshot"`
	Previous string    `json:"previous"`
	State    State     `json:"state"`
	Attempts []Attempt `json:"attempts"`
}

// File is one metadata file of a snapshot, recorded by its live key.
type File struct {
	Key    string `json:"key"`
	Size   int64  `json:"size"`
	ETag   string `json:"etag"`
	SHA256 string `json:"sha256"`
}

// Manifest lists the files of a complete snapshot. Tag and TagDate are empty for AdoptedSnapshot.
type Manifest struct {
	Schema  int    `json:"schema"`
	Tag     string `json:"tag"`
	TagDate string `json:"tag_date"`
	Files   []File `json:"files"`
}

func encode(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err) // only plain structs are encoded
	}
	return append(b, '\n')
}

// decode checks the schema first, so a newer body fails on the version and not on a field.
// json.Unmarshal of the head also rejects trailing data.
func decode(what string, body []byte, v any) error {
	var head struct {
		Schema *int `json:"schema"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if head.Schema == nil || *head.Schema != Schema {
		got := "none"
		if head.Schema != nil {
			got = fmt.Sprint(*head.Schema)
		}
		return fmt.Errorf("%s: schema %s, this build knows %d", what, got, Schema)
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// ParseLock decodes and checks a lock body.
func ParseLock(body []byte) (Lock, error) {
	var l Lock
	if err := decode("lock", body, &l); err != nil {
		return Lock{}, err
	}
	if !l.State.valid() {
		return Lock{}, fmt.Errorf("lock: unknown state %q", l.State)
	}
	if l.Tag == "" || l.Snapshot == "" || l.Previous == "" {
		return Lock{}, fmt.Errorf("lock: tag, snapshot and previous are required")
	}
	return l, nil
}

// ParseManifest decodes and checks a manifest body.
func ParseManifest(body []byte) (Manifest, error) {
	var m Manifest
	if err := decode("manifest", body, &m); err != nil {
		return Manifest{}, err
	}
	return m, nil
}
