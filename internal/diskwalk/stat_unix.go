//go:build unix

package diskwalk

import (
	"io/fs"
	"syscall"
)

func stat(fi fs.FileInfo) fileStat {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileStat{alloc: uint64(max(fi.Size(), 0))}
	}
	return fileStat{
		id:    fileID{dev: uint64(st.Dev), ino: uint64(st.Ino)},
		links: uint64(st.Nlink),
		alloc: uint64(max(st.Blocks, 0)) * 512,
	}
}
