//go:build linux

package ui

import "golang.org/x/sys/unix"

// flushTTYReadQueue discards bytes queued in the terminal's input buffer.
// On Linux this is the TCFLSH ioctl with TCIFLUSH passed as a direct integer.
func flushTTYReadQueue(fd uintptr) {
	_ = unix.IoctlSetInt(int(fd), unix.TCFLSH, unix.TCIFLUSH) //nolint:errcheck
}
