package release

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseTag(t *testing.T) {
	for s, want := range map[string]Tag{
		"v26.3.36.6-lts":            {Name: "v26.3.36.6-lts", Major: 26, Minor: 3, Patch: 36, Build: 6, Channels: []string{"lts", "stable"}},
		"v26.7.19.5-stable":         {Name: "v26.7.19.5-stable", Major: 26, Minor: 7, Patch: 19, Build: 5, Channels: []string{"stable"}},
		"v0.0.0.0-stable":           {Name: "v0.0.0.0-stable", Channels: []string{"stable"}},
		"v4294967295.1.2.3-lts":     {Name: "v4294967295.1.2.3-lts", Major: 4294967295, Minor: 1, Patch: 2, Build: 3, Channels: []string{"lts", "stable"}},
		"v1.4294967295.0.10-stable": {Name: "v1.4294967295.0.10-stable", Major: 1, Minor: 4294967295, Build: 10, Channels: []string{"stable"}},
	} {
		got, err := ParseTag(s)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("ParseTag(%q) = %+v, %v; want %+v", s, got, err, want)
		}
	}
	got, _ := ParseTag("v26.3.36.6-lts")
	if got.Version() != "26.3.36.6" {
		t.Errorf("version %q", got.Version())
	}
	for _, s := range []string{
		"", "v26.3.36.6", "26.3.36.6-lts", "v26.3.36-lts", "v26.3.36.6.1-lts",
		"v26.3.36.6-testing", "v26.3.36.6-LTS", "v26.3.36.6-lts ", " v26.3.36.6-lts",
		"v26.3.36.6-lts\n", "v026.3.36.6-lts", "v26.03.36.6-lts", "v26.3.36.00-lts",
		"v4294967296.0.0.0-lts", "v99999999999999999999.0.0.0-lts", "v+26.3.36.6-lts",
		"v26.3.36.6-lts-stable", "v26.3.-36.6-lts", "v٢٦.3.36.6-lts", "V26.3.36.6-lts",
		"v26.3.36.6_lts", "v26..36.6-lts", "refs/tags/v26.3.36.6-lts",
	} {
		if _, err := ParseTag(s); err == nil {
			t.Errorf("ParseTag(%q) accepted", s)
		}
	}
}

func TestParseDate(t *testing.T) {
	d, err := ParseDate("2026-09-29T12:29:02Z")
	if err != nil || !d.Equal(time.Date(2026, 9, 29, 12, 29, 2, 0, time.UTC)) {
		t.Fatalf("%v %v", d, err)
	}
	for _, s := range []string{
		"", "2026-09-29", "2026-09-29T12:29:02", "2026-09-29T12:29:02+00:00",
		"2026-09-29T12:29:02.5Z", "2026-09-29 12:29:02Z", "2026-13-01T00:00:00Z",
		"2026-02-30T00:00:00Z", "2026-09-29T24:00:00Z", "2026-09-29t12:29:02z",
		" 2026-09-29T12:29:02Z", "2026-09-29T12:29:02Z\n", "20260929T122902Z", "1727612942",
	} {
		if _, err := ParseDate(s); err == nil {
			t.Errorf("ParseDate(%q) accepted", s)
		}
	}
}

func TestSnapshotName(t *testing.T) {
	tag, _ := ParseTag("v26.3.36.6-lts")
	d, _ := ParseDate("2026-09-29T12:29:02Z")
	if got := SnapshotName(tag, d); got != "20260929T122902Z-v26.3.36.6-lts" {
		t.Fatal(got)
	}
	if AdoptedSnapshot >= SnapshotName(tag, time.Time{}) {
		t.Fatal("the adopted snapshot does not sort first")
	}
}

func TestLockBody(t *testing.T) {
	l := Lock{Schema: 1, Tag: "v26.3.36.6-lts", TagDate: "2026-09-29T12:29:02Z", Snapshot: "s", Previous: AdoptedSnapshot, State: Locked,
		Attempts: []Attempt{{RunID: "1", RunURL: "https://x/1", Started: "2026-09-29T13:00:00Z", Version: "dev"}}}
	got, err := ParseLock(encode(l))
	if err != nil || !reflect.DeepEqual(got, l) {
		t.Fatalf("%+v %v", got, err)
	}
	for name, tc := range map[string]struct{ body, err string }{
		"schema 2":      {`{"schema":2,"tag":"t","future":1}`, "schema 2"},
		"no schema":     {`{"tag":"t"}`, "schema none"},
		"string schema": {`{"schema":"1"}`, "lock"},
		"unknown field": {strings.Replace(string(encode(l)), `"state"`, `"extra":1,"state"`, 1), "unknown field"},
		"unknown state": {strings.Replace(string(encode(l)), `"locked"`, `"open"`, 1), "unknown state"},
		"trailing":      {string(encode(l)) + "{}", "after top-level value"},
		"not json":      {"lock", "lock"},
	} {
		if _, err := ParseLock([]byte(tc.body)); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: %v, want %q", name, err, tc.err)
		}
	}
	if _, err := ParseManifest([]byte(`{"schema":3,"files":[]}`)); err == nil {
		t.Error("manifest schema 3 accepted")
	}
}
