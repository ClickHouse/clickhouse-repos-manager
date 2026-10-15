// Package release holds the release protocol: the global lock, adopt and snapshots.
package release

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// Tag is a parsed release tag, for example v26.3.36.6-lts.
type Tag struct {
	Name                       string
	Major, Minor, Patch, Build uint32
	// Channels lists the channels the tag publishes to.
	Channels []string
}

// Version is the bare version in the package file names, for example 26.3.36.6.
func (t Tag) Version() string {
	return fmt.Sprintf("%d.%d.%d.%d", t.Major, t.Minor, t.Patch, t.Build)
}

var tagRE = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-(lts|stable)$`)

// ParseTag accepts only v<major>.<minor>.<patch>.<build>-lts or -stable.
func ParseTag(s string) (Tag, error) {
	m := tagRE.FindStringSubmatch(s)
	if m == nil {
		return Tag{}, fmt.Errorf("tag %q: want v<major>.<minor>.<patch>.<build>-lts or -stable", s)
	}
	var n [4]uint32
	for i := range n {
		v, err := strconv.ParseUint(m[i+1], 10, 32)
		if err != nil {
			return Tag{}, fmt.Errorf("tag %q: %s does not fit uint32", s, m[i+1])
		}
		n[i] = uint32(v)
	}
	t := Tag{Name: s, Major: n[0], Minor: n[1], Patch: n[2], Build: n[3]}
	if m[5] == "lts" {
		t.Channels = []string{"lts", "stable"}
	} else {
		t.Channels = []string{"stable"}
	}
	return t, nil
}

// DateFormat is the tag date on the command line and in JSON bodies.
const DateFormat = "2006-01-02T15:04:05Z"

// compactDate starts every snapshot and assets name.
const compactDate = "20060102T150405Z"

var dateRE = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)

// ParseDate accepts only YYYY-MM-DDTHH:MM:SSZ. It does not compare with the clock.
func ParseDate(s string) (time.Time, error) {
	if !dateRE.MatchString(s) {
		return time.Time{}, fmt.Errorf("date %q: want YYYY-MM-DDTHH:MM:SSZ", s)
	}
	d, err := time.Parse(DateFormat, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("date %q: %w", s, err)
	}
	return d, nil
}

// AdoptedSnapshot is the snapshot of the live state found by the first lock.
// The zero date sorts it first.
const AdoptedSnapshot = "00000000T000000Z-adopted"

// SnapshotName is "<compact tag date>-<tag>", for example 20260929T122902Z-v26.3.36.6-lts.
func SnapshotName(t Tag, date time.Time) string {
	return date.UTC().Format(compactDate) + "-" + t.Name
}
