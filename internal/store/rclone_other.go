// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package store

import (
	"os"
	"syscall"
)

// childProcAttr has nothing to set where the process-group and parent-death flags do not
// exist; see the Linux version for what they are for.
func childProcAttr() *syscall.SysProcAttr { return nil }

// terminate ends a process the bluntest way, which is all that is portable.
func terminate(p *os.Process) error { return p.Kill() }
