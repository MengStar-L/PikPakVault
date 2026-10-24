package aria2

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSparseDownloadReservesUnallocatedBytes(t *testing.T) {
	dir := t.TempDir()
	available, err := freeSpace(dir)
	if err != nil {
		t.Fatal(err)
	}
	size := int64(available + 1<<30)
	f, err := os.Create(filepath.Join(dir, "content"))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(size); err != nil {
		f.Close()
		t.Skipf("filesystem cannot create this sparse test file: %v", err)
	}
	f.Close()
	info, err := os.Stat(filepath.Join(dir, "content"))
	if err != nil || allocatedBytes(info) >= size {
		t.Fatal("expected a sparse file", err)
	}
	var low *InsufficientSpaceError
	if err = CheckSpace(dir, size); !errors.As(err, &low) || low.Needed < uint64(size)/2 {
		t.Fatal("counted holes as downloaded bytes", err, low)
	}
	if err = writeJSON(filepath.Join(dir, "source.json"), metadata{Size: size, Complete: true}); err != nil {
		t.Fatal(err)
	}
	if err = CheckSpace(dir, size); err != nil {
		t.Fatal("completed cached data should not reserve a second copy", err)
	}
}
