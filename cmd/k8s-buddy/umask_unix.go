//go:build !windows

package main

import "syscall"

// privateFiles makes every file this process (and the kubectl/az processes it starts) creates
// readable by the user only: the usage database's -wal/-shm files, caches, temp files.
func privateFiles() { syscall.Umask(0o077) }
