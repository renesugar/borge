// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the subprocess handling in borgstore/backends/rclone.py
// (borgstore 0.6.1, BSD 3-Clause, Copyright (C) 2026 Thomas Waldmann).
// Licensed under the BSD 3-Clause License, see licenses/upstream-python/borgstore.LICENSE.rst.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package store

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"
)

// rcloneStartTimeout bounds the wait for the server to listen. It is generous because the
// first run of rclone on a cold cache is slow, and short enough that a wedged rclone does
// not hang a backup indefinitely.
const rcloneStartTimeout = 30 * time.Second

// rcloneProcess is a child process borge started and is responsible for stopping.
//
// The exit is watched from the moment it starts, for two reasons: waiting for the port has
// to notice a server that died instead of listening, and a process that is never reaped is
// a zombie for as long as borge runs.
type rcloneProcess struct {
	cmd     *exec.Cmd
	done    chan struct{}
	exitErr error
}

// startProcess runs a program with extra environment variables, discarding its output.
//
// The output goes nowhere on purpose: rclone logs to stderr, and a backup's stderr belongs
// to borge. What matters from the child is whether it listens, which is measured directly.
func startProcess(name string, args []string, env ...string) (*rcloneProcess, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = childProcAttr()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &rcloneProcess{cmd: cmd, done: make(chan struct{})}
	go func() {
		p.exitErr = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// waitForPort reports whether the server began listening before it exited or timed out.
func (p *rcloneProcess) waitForPort(addr string) bool {
	deadline := time.Now().Add(rcloneStartTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			return false
		default:
		}
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// wait blocks until the process has exited and reports how.
func (p *rcloneProcess) wait() error {
	<-p.done
	return p.exitErr
}

// stop asks the process to exit, and insists if it will not.
func (p *rcloneProcess) stop(grace time.Duration) error {
	if err := terminate(p.cmd.Process); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("store: could not stop rclone: %w", err)
	}
	select {
	case <-p.done:
	case <-time.After(grace):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
	// A server that was told to exit reports a signal as its exit status. That is the
	// outcome that was asked for, so it is not an error to hand back.
	return nil
}
