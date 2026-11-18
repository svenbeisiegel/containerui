//go:build !unix

package diskwalk

import "io/fs"

// stat uses the file size: there is no portable allocated size or file ID
// here, so hard links are counted for every link.
func stat(fi fs.FileInfo) fileStat {
	return fileStat{alloc: uint64(max(fi.Size(), 0))}
}
