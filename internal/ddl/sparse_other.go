//go:build !windows

package ddl

import "os"

// makeSparse is a no-op: on Linux and macOS, extending a file with truncate
// already leaves the gap unallocated.
func makeSparse(*os.File) error { return nil }
