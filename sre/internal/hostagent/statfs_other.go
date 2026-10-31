//go:build !linux

package hostagent

import "errors"

// statfsMi is only implemented on Linux, the only platform the host agent
// ships for; elsewhere disks are simply not reported.
func statfsMi(string) (int64, int64, error) {
	return 0, 0, errors.New("statfs: unsupported platform")
}

func fileOwner(string) int { return -1 }
