package aria2

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestInstallerRejectsModifiedArchiveAndExecutable(t *testing.T) {
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	w, _ := z.Create("aria2c")
	w.Write([]byte("fixture executable"))
	z.Close()
	body := buf.Bytes()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer server.Close()
	d := distribution{URL: server.URL, ArchiveHash: hash(body), BinaryHash: hash([]byte("fixture executable")), Entry: "aria2c"}
	home := t.TempDir()
	bad := d
	bad.ArchiveHash = strings.Repeat("0", 64)
	if _, e := install(context.Background(), home, bad, server.Client()); e == nil {
		t.Fatal("unchecked archive installed")
	}
	bad = d
	bad.BinaryHash = strings.Repeat("0", 64)
	if _, e := install(context.Background(), home, bad, server.Client()); e == nil {
		t.Fatal("unchecked executable installed")
	}
	if _, e := os.Stat(executable(home)); !os.IsNotExist(e) {
		t.Fatal("failed verification left executable")
	}
	file, e := install(context.Background(), home, d, server.Client())
	if e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(file)
	if string(b) != "fixture executable" {
		t.Fatal("wrong extraction")
	}
}
