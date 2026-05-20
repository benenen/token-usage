//go:build !unix

package watcher

import "os"

// On non-unix platforms we fall back to size-based detection only.
// The watcher resets offset when file size shrinks below saved offset.
func inodeOf(_ os.FileInfo) uint64 { return 0 }
