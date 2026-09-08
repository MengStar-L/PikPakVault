// Package creates Linux bundles with consistent permissions on Windows and Linux.
package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: package <bundle-directory> <archive.tar.gz>")
		os.Exit(2)
	}
	if err := pack(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func pack(dir, destination string) (err error) {
	f, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer func() {
		if e := f.Close(); err == nil {
			err = e
		}
		if err != nil {
			_ = os.Remove(destination)
		}
	}()
	gz := gzip.NewWriter(f)
	defer func() {
		if e := gz.Close(); err == nil {
			err = e
		}
	}()
	tw := tar.NewWriter(gz)
	defer func() {
		if e := tw.Close(); err == nil {
			err = e
		}
	}()
	return filepath.WalkDir(dir, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, e := filepath.Rel(dir, name)
		if e != nil || rel == "." {
			return e
		}
		info, e := entry.Info()
		if e != nil {
			return e
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported bundle entry: %s", rel)
		}
		h, e := tar.FileInfoHeader(info, "")
		if e != nil {
			return e
		}
		h.Name = filepath.ToSlash(rel)
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		h.Mode = 0644
		if info.IsDir() || h.Name == "vault" || strings.HasSuffix(h.Name, ".sh") {
			h.Mode = 0755
		}
		if e = tw.WriteHeader(h); e != nil || info.IsDir() {
			return e
		}
		src, e := os.Open(name)
		if e != nil {
			return e
		}
		_, e = io.Copy(tw, src)
		closeErr := src.Close()
		if e != nil {
			return e
		}
		return closeErr
	})
}
