//go:build linux

package hostagent

import "syscall"

// statfsMi returns a filesystem's size and used space in MiB (used counts
// blocks not available to unprivileged users, like df).
func statfsMi(path string) (totalMi, usedMi int64, err error) {
	var st syscall.Statfs_t
	if err = syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := int64(st.Bsize)
	total := int64(st.Blocks) * bs
	avail := int64(st.Bavail) * bs
	free := int64(st.Bfree) * bs
	used := total - free
	// Size as df reports it: used + available to users.
	if used+avail > 0 {
		total = used + avail
	}
	return total >> 20, used >> 20, nil
}
