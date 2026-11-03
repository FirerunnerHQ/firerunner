//go:build !unix

package cachetls

import "os"

func owner(os.FileInfo) (int, bool) { return 0, false }
