//go:build windows

package main

import "syscall"

// autopushDetachedAttr returns nil on Windows because the cmd.Start() default
// behaviour already detaches the child from the parent's signal group.
// (Windows has no process-group concept comparable to POSIX.)
func autopushDetachedAttr() *syscall.SysProcAttr {
	return nil
}
