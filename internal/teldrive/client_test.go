package teldrive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTreePaginationRangesAndCredentials(t *testing.T) {
	broken := false
	requests := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		cookie, e := r.Cookie("access_token")
		if e != nil || cookie.Value != "test-token" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("credentials missing")
		}
		switch r.URL.Path {
		case "/api/files/folder":
			json.NewEncoder(w).Encode(File{ID: "folder", Name: "source", Type: "folder"})
		case "/api/files":
			if r.URL.Query().Get("sort") != "id" {
				t.Error("unstable pagination sort")
			}
			p := Page{}
			p.Items = []File{}
			p.Meta.Current = 1
			switch r.URL.Query().Get("parentId") {
			case "folder":
				p.Meta.Count = 2
				p.Meta.Pages = 2
				if r.URL.Query().Get("page") == "2" {
					if broken {
						http.Error(w, "test-token", 503)
						return
					}
					p.Meta.Current = 2
					p.Items = []File{{ID: "movie", Name: "视频.mp4", Type: "file", ParentID: "folder", Size: 6}}
				} else {
					p.Items = []File{{ID: "sub", Name: "子目录", Type: "folder", ParentID: "folder"}}
				}
			case "sub":
				p.Meta.Count = 1
				p.Meta.Pages = 1
				p.Items = []File{{ID: "text", Name: "note.txt", Type: "file", ParentID: "sub", Size: 3}}
			}
			json.NewEncoder(w).Encode(p)
		case "/api/files/movie/视频.mp4":
			if r.Header.Get("Range") != "bytes=2-4" {
				t.Error("bad range")
			}
			w.Header().Set("Content-Range", "bytes 2-4/6")
			w.WriteHeader(206)
			io.WriteString(w, "cde")
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	c, _ := New(s.URL+"/api/", "test-token")
	entries, e := c.Tree(context.Background(), "folder")
	if e != nil || len(entries) != 3 {
		t.Fatalf("tree: %v %+v", e, entries)
	}
	if entries[1].Path != "子目录/note.txt" {
		t.Fatal(entries)
	}
	r, e := c.OpenRange(context.Background(), File{ID: "movie", Name: "视频.mp4", Size: 6}, 2, 3)
	if e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(r)
	r.Close()
	if e != nil || string(b) != "cde" {
		t.Fatal(string(b), e)
	}
	broken = true
	if _, e = c.Tree(context.Background(), "folder"); e == nil || strings.Contains(e.Error(), "test-token") {
		t.Fatal("incomplete tree accepted or credential leaked", e)
	}
	if requests < 6 {
		t.Fatal(requests)
	}
}

func TestRefuseRedirectAndMalformedRange(t *testing.T) {
	for _, mode := range []string{"redirect", "range", "duplicate", "wrong-parent"} {
		t.Run(mode, func(t *testing.T) {
			leak := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("followed redirect with credentials") }))
			defer leak.Close()
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "redirect" {
					http.Redirect(w, r, leak.URL, 302)
					return
				}
				if mode == "range" {
					w.WriteHeader(200)
					io.WriteString(w, "abc")
					return
				}
				p := Page{Items: []File{{ID: "a", Name: "file", Type: "file"}}}
				p.Meta.Current = 1
				p.Meta.Count = 1
				p.Meta.Pages = 1
				if mode == "duplicate" {
					p.Items = append(p.Items, p.Items[0])
					p.Meta.Count = 2
				} else {
					p.Items[0].ParentID = "outside"
				}
				json.NewEncoder(w).Encode(p)
			}))
			defer s.Close()
			c, _ := New(s.URL, "secret")
			var e error
			if mode == "redirect" || mode == "range" {
				_, e = c.OpenRange(context.Background(), File{ID: "id", Name: "file", Size: 6}, 2, 3)
			} else {
				_, e = c.Tree(context.Background(), "")
			}
			if e == nil {
				t.Fatal(fmt.Sprintf("accepted %s", mode))
			}
		})
	}
}
