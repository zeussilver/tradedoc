// Package blob stores uploaded files. Local disk for now; an S3
// implementation of the same interface replaces it on day 6.
package blob

import (
	"context"
	"io"
	"os"
	"path/filepath"
)

type Store interface {
	Put(ctx context.Context, key string, r io.Reader) error
}

type Local struct{ Dir string }

// Put writes the file under Dir. Keys are server-generated IDs, never user
// filenames, so a name like "../../etc/passwd" can't escape the folder.
func (l Local) Put(_ context.Context, key string, r io.Reader) error {
	path := filepath.Join(l.Dir, filepath.Base(key))
	if err := os.MkdirAll(l.Dir, 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}
