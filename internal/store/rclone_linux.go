// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"os"
	"syscall"
)

// childProcAttr puts the rclone server in its own process group and has the kernel signal
// it if borge dies.
//
// The process group is borgstore's ignore_sigint by another route: a Ctrl-C at a terminal
// goes to the foreground *group*, and rclone should not be torn down underneath a backup
// that is still deciding how to stop. Pdeathsig is borge's own addition, and it closes the
// gap borgstore leaves: if borge is killed outright, nothing else would ever stop the
// rclone it started, and the user would be left with a stray server holding their storage
// credentials.
func childProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
}

// terminate asks a process to exit. SIGTERM rather than SIGINT, because the child is
// deliberately shielded from the interrupt.
func terminate(p *os.Process) error { return p.Signal(syscall.SIGTERM) }
