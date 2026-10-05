package journal

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// OpenSource pins each directory and refuses symlinks while opening the fixed
// journal location, including replacements between checking and opening.
func OpenSource(path string, flags int, perm os.FileMode) (*os.File, error) {
	if filepath.Base(path) != "events.jsonl" || filepath.Base(filepath.Dir(path)) != "journal" {
		return nil, errors.New("invalid repository journal location")
	}
	storePath := filepath.Dir(filepath.Dir(path))
	root, err := os.Open(filepath.Dir(storePath))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	storeFD, err := syscall.Openat(int(root.Fd()), filepath.Base(storePath), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(storeFD)
	journalFD, err := syscall.Openat(storeFD, "journal", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(journalFD)
	fd, err := syscall.Openat(journalFD, "events.jsonl", flags|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, uint32(perm.Perm()))
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("journal must be a regular file")
	}
	return file, nil
}
