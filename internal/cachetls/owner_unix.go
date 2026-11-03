//go:build unix

package cachetls

import (
	"os"
	"syscall"
)

// owner is fi's owning uid, where the platform tells it.
func owner(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
