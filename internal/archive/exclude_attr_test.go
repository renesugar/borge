// SPDX-License-Identifier: Apache-2.0

package archive

import "testing"

// excludedByAttr is unit-tested as well as compared against borg, because one of its three
// rules cannot be reached from a Linux filesystem at all: Linux permits only the user,
// security, system and trusted xattr namespaces, and borg's Apple marker is in none of
// them. It still arrives on archives made on macOS and on anything imported from a tar
// carrying it, so the rule is real and only its *source* is out of reach here.
//
// The asymmetries are the point. Present-at-all excludes for one attribute; an exact value
// excludes for another; a set-but-different value must not.
func TestExcludedByAttr(t *testing.T) {
	nodump := int64(bsdNoDump)
	immutable := int64(bsdImmutable)
	none := int64(0)

	cases := []struct {
		name   string
		xattrs map[string][]byte
		flags  *int64
		want   bool
	}{
		{"nothing at all", nil, nil, false},
		{"no markers", map[string][]byte{"user.other": []byte("x")}, &none, false},

		{"apple marker, empty value", map[string][]byte{xattrAppleExclude: {}}, nil, true},
		{"apple marker, any value", map[string][]byte{xattrAppleExclude: []byte("plist")}, nil, true},

		{"xdg false", map[string][]byte{xattrXDGBackup: []byte("false")}, nil, true},
		// The attribute exists to say "back this up", so anything but "false" keeps the
		// file. Reading it as a mere presence check would silently drop files whose owner
		// had asked for the opposite.
		{"xdg true", map[string][]byte{xattrXDGBackup: []byte("true")}, nil, false},
		{"xdg empty", map[string][]byte{xattrXDGBackup: {}}, nil, false},
		{"xdg other", map[string][]byte{xattrXDGBackup: []byte("0")}, nil, false},
		{"xdg False capitalised", map[string][]byte{xattrXDGBackup: []byte("False")}, nil, false},

		{"nodump", nil, &nodump, true},
		{"nodump among others", nil, func() *int64 { v := nodump | immutable; return &v }(), true},
		{"immutable alone", nil, &immutable, false},
		{"flags examined, none set", nil, &none, false},
		{"flags not examined", nil, nil, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := excludedByAttr(c.xattrs, c.flags); got != c.want {
				t.Errorf("excludedByAttr(%v, %v) = %v, want %v", c.xattrs, c.flags, got, c.want)
			}
		})
	}
}
