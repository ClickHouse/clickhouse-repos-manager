// Package deb is the DEB package type: .deb files in deb/pool/, indexed per
// codename and architecture in deb/dists/, as reprepro laid them out.
package deb

import (
	"fmt"
	"strings"
)

const suffix = ".deb"

// Architectures of a file name; all goes into the index of each of the others.
var archs = map[string]bool{"amd64": true, "arm64": true, "all": true}

// ParseName takes <package>_<version>_<arch>.deb. ok claims the name by its
// suffix; err means it is a .deb that does not parse for version.
func ParseName(name, version string) (pkg, arch string, ok bool, err error) {
	base, ok := strings.CutSuffix(name, suffix)
	if !ok {
		return "", "", false, nil
	}
	want := fmt.Errorf("%s: want <package>_%s_<arch>%s", name, version, suffix)
	i := strings.LastIndexByte(base, '_')
	if i < 0 {
		return "", "", true, want
	}
	if arch = base[i+1:]; !archs[arch] {
		return "", "", true, fmt.Errorf("%s: unknown architecture %q, want amd64, arm64 or all", name, arch)
	}
	if pkg, ok = strings.CutSuffix(base[:i], "_"+version); !ok || !validName(pkg) {
		return "", "", true, want
	}
	return pkg, arch, true, nil
}

// validName is the Debian package name rule: [a-z0-9][a-z0-9+.-]+.
func validName(s string) bool {
	if len(s) < 2 {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case i > 0 && (c == '+' || c == '.' || c == '-'):
		default:
			return false
		}
	}
	return true
}

// SourceName returns the source package name of a control paragraph: the
// Source field without its optional "(version)", or Package without Source.
func SourceName(p Paragraph) (string, error) {
	src, ok := p.Get("Source")
	if !ok {
		return p.Value("Package"), nil
	}
	name, rest, _ := strings.Cut(src, " ")
	if rest != "" {
		v, ok := strings.CutPrefix(rest, "(")
		if v, ok = strings.CutSuffix(v, ")"); !ok || v == "" || strings.ContainsAny(v, " ()") {
			return "", fmt.Errorf("Source %q: want <name> or <name> (<version>)", src)
		}
	}
	if !validName(name) {
		return "", fmt.Errorf("Source %q: %q is no valid package name", src, name)
	}
	return name, nil
}

// PoolPath is the Filename of a package, relative to deb/: reprepro's
// pool/main/<prefix>/<source>/<file>, where the prefix is the first letter of
// the source, or its first four for lib* sources.
func PoolPath(source, file string) string {
	prefix := source[:1]
	if strings.HasPrefix(source, "lib") && len(source) > 3 {
		prefix = source[:4]
	}
	return "pool/main/" + prefix + "/" + source + "/" + file
}
