package engine

import (
	"fmt"
	"strconv"
	"strings"
)

// fileIdentity tells one file from another that later appears at the same
// path. created is the file's creation (birth) time in nanoseconds, 0 if
// the filesystem doesn't report it; index is its inode or NTFS file index
// (which includes a reuse counter on NTFS), 0 if unknown.
//
// Matching (same): both must agree wherever both are known. Inode numbers
// get reused (ext4, Samba shares on ext4), so the creation time tells a new
// file at the same inode apart; creation times can be coarse or placeholders
// (FAT, some network shares), so the index tells those apart. On Windows the
// index is required: NTFS gives a file recreated under the same name within
// 15 s the old one's creation time ("tunneling"). With neither, nothing
// matches. On drives whose inode numbers change across mounts (FAT on
// Linux), a torrent may stop after a remount: safe, if inconvenient.
type fileIdentity struct {
	index   uint64
	created int64
}

// known: Windows needs the file index (see same); elsewhere either will do.
// A creation time at or before 1970 is a filesystem's placeholder, not one.
func (a fileIdentity) known() bool {
	if bothNeeded {
		return a.index != 0
	}
	return a.index != 0 || a.created > 0
}

// same also requires the index to match wherever both sides have one: some
// filesystems (FAT, exFAT, network shares) report coarse or placeholder
// creation times, so the time alone could match a different file. Losing
// ownership after a remount only stops the torrent; a false match could
// overwrite or delete someone else's file.
func (a fileIdentity) same(b fileIdentity) bool {
	if a.index != 0 && b.index != 0 && a.index != b.index {
		return false
	}
	if a.created > 0 && b.created > 0 {
		if a.created != b.created {
			return false
		}
		if bothNeeded {
			return a.index != 0 && a.index == b.index
		}
		return true
	}
	return a.index != 0 && a.index == b.index
}

func (a fileIdentity) String() string {
	return strconv.FormatUint(a.index, 16) + ":" + strconv.FormatInt(a.created, 10)
}

func parseIdentity(s string) (fileIdentity, error) {
	i, c, ok := strings.Cut(s, ":")
	if !ok {
		return fileIdentity{}, fmt.Errorf("bad identity %q", s)
	}
	index, err := strconv.ParseUint(i, 16, 64)
	if err != nil {
		return fileIdentity{}, err
	}
	created, err := strconv.ParseInt(c, 10, 64)
	if err != nil {
		return fileIdentity{}, err
	}
	return fileIdentity{index, created}, nil
}

// FileID is the identity of the file at path, to tell it later from another
// file put at the same path (direct downloads use it; see SameFileID).
func FileID(path string) (string, error) {
	id, err := identityOf(path)
	if err != nil {
		return "", err
	}
	if !id.known() {
		return "", fmt.Errorf("%s: this drive doesn't tell files apart", path)
	}
	return id.String(), nil
}

// SameFileID reports whether the file at path is the one FileID identified.
func SameFileID(id, path string) bool {
	want, err := parseIdentity(id)
	if err != nil {
		return false
	}
	got, err := identityOf(path)
	return err == nil && want.same(got)
}
