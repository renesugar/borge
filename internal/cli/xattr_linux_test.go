// SPDX-License-Identifier: Apache-2.0

//go:build linux

package cli

import "golang.org/x/sys/unix"

// setTestXattr sets an extended attribute, for the tests that need one.
func setTestXattr(path, name, value string) error {
	return unix.Lsetxattr(path, name, []byte(value), 0)
}
