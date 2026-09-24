package app

import (
	"os"
	"syscall"
)

func integrityFileStateOf(info os.FileInfo) (integrityFileState, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return integrityFileState{}, false
	}
	return integrityFileState{
		Dev:     uint64(stat.Dev),
		Ino:     uint64(stat.Ino),
		Size:    stat.Size,
		MtimeNS: stat.Mtim.Nano(),
		CtimeNS: stat.Ctim.Nano(),
	}, true
}
