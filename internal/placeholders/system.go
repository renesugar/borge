// SPDX-License-Identifier: Apache-2.0

package placeholders

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/user"
	"strconv"
)

// osUsername is the name the current user is filed under.
//
// os/user falls back to the passwd database and then to the USER environment, and can
// fail outright in a container with no passwd entry. A backup should not stop for that,
// so the numeric uid stands in - it is still an identifier, which is what the placeholder
// is for.
func osUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return strconv.Itoa(os.Getuid())
}

// uuid4 is a random version-4 UUID.
//
// Written out rather than taken from a dependency: it is sixteen random bytes with six
// bits set, and google/uuid would be a module added for one placeholder.
func uuid4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing means the machine is in no state to be making backups, but
		// a placeholder is not where that should be diagnosed.
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 1
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
