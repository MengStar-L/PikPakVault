package aria2

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSpaceShortageIsTypedAndDoesNotCreateCache(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-created", "cache")
	var low *InsufficientSpaceError
	if err := CheckSpace(dir, 1<<62); !errors.As(err, &low) || low.Needed != uint64(1<<62)+(64<<20) || low.Available >= low.Needed {
		t.Fatal("missing recoverable space information", err, low)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("space preflight mutated the cache", err)
	}
}

func TestAria2DiskFullCodeCanWaitButNetworkErrorsCannot(t *testing.T) {
	dir := t.TempDir()
	var low *InsufficientSpaceError
	if err := downloadError("9", "", dir, 1<<62); !errors.As(err, &low) || low.Available >= low.Needed {
		t.Fatal("disk full was not classified", err)
	}
	if err := downloadError("9", "", dir, 1); !errors.As(err, &low) || !low.DuringDownload {
		t.Fatal("recently released space lost aria2's failure classification", err)
	}
	if err := downloadError("5", "source stalled", dir, 1); errors.As(err, &low) {
		t.Fatal("unrelated download failure retried as disk full", err)
	}
}
