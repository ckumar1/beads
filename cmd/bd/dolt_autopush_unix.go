//go:build !windows

package main

import "syscall"

// autopushDetachedAttr puts the spawned worker in its own process group so
// the parent's exit doesn't propagate SIGHUP to the worker. Modeled on
// doltserver.procAttrDetached.
func autopushDetachedAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
