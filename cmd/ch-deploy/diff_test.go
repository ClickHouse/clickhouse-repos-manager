package main

import (
	"strings"
	"testing"
)

func TestUnifiedDiff(t *testing.T) {
	lines := func(s ...string) []byte { return []byte(strings.Join(s, "\n") + "\n") }
	for name, tc := range map[string]struct {
		a, b []byte
		want string
	}{
		"equal": {lines("a"), lines("a"), ""},
		"append": {lines("1", "2", "3", "4", "5"), lines("1", "2", "3", "4", "5", "6", "7"),
			"@@ -3,3 +3,5 @@\n 3\n 4\n 5\n+6\n+7\n"},
		"new file": {nil, lines("x", "y"), "@@ -0,0 +1,2 @@\n+x\n+y\n"},
		"two hunks": {lines("a", "1", "2", "3", "4", "5", "6", "7", "8", "b"), lines("A", "1", "2", "3", "4", "5", "6", "7", "8", "B"),
			"@@ -1,4 +1,4 @@\n-a\n+A\n 1\n 2\n 3\n@@ -7,4 +7,4 @@\n 6\n 7\n 8\n-b\n+B\n"},
		"one hunk when close": {lines("a", "1", "2", "b"), lines("A", "1", "2", "B"),
			"@@ -1,4 +1,4 @@\n-a\n+A\n 1\n 2\n-b\n+B\n"},
		"no newline": {[]byte("a\nb"), []byte("a\nc"), "@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+c\n\\ No newline at end of file\n"},
		"in the middle": {lines("Date: 1", "MD5Sum:", " x 1 a", " y 2 b"), lines("Date: 2", "MD5Sum:", " x 1 a", " z 3 b"),
			"@@ -1,4 +1,4 @@\n-Date: 1\n+Date: 2\n MD5Sum:\n  x 1 a\n- y 2 b\n+ z 3 b\n"},
	} {
		t.Run(name, func(t *testing.T) {
			got := unifiedDiff("f", tc.a, tc.b)
			if tc.want != "" {
				tc.want = "--- a/f\n+++ b/f\n" + tc.want
			}
			if got != tc.want {
				t.Fatalf("got\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}
