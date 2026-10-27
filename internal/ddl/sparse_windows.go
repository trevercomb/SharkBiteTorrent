package ddl

import (
	"os"

	"golang.org/x/sys/windows"
)

// makeSparse marks a file sparse, so writing a later segment far past the
// end doesn't make Windows zero-fill the gap first, which over SMB means
// sending gigabytes of zeros to the NAS before the first real byte (seen
// live: eight downloads stalled at 0% while their part files grew). The
// torrent engine does the same (internal/engine/sparse_windows.go).
func makeSparse(f *os.File) error {
	var n uint32
	return windows.DeviceIoControl(windows.Handle(f.Fd()), windows.FSCTL_SET_SPARSE, nil, 0, nil, 0, &n, nil)
}
