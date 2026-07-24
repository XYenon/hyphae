//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package ui

import "golang.org/x/sys/unix"

// fread is the FREAD flag bit (from <sys/fcntl.h>); TIOCFLUSH interprets it as
// "flush the read queue". It is not exported by x/sys/unix, so define it here.
const fread = 0x1

// flushTTYReadQueue discards bytes queued in the terminal's input buffer.
// On BSD/darwin this is the TIOCFLUSH ioctl, which takes a pointer to an int
// whose FREAD/FWRITE bits select the queues to flush.
func flushTTYReadQueue(fd uintptr) {
	_ = unix.IoctlSetPointerInt(int(fd), unix.TIOCFLUSH, fread) //nolint:errcheck
}
