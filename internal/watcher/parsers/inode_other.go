//go:build !unix

package parsers

import "os"

// On non-unix platforms we don't have stable inodes. The Scanner falls
// back to size-based change detection in that case.
func inodeOf(_ os.FileInfo) uint64 { return 0 }
