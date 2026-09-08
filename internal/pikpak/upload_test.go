package pikpak

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBeginUploadUsesFinalDestinationAndGCID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/drive/v1/files" {
			t.Fatal(r.Method, r.URL.Path)
		}
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		if payload["parent_id"] != "target" || payload["name"] != "video.mp4" || payload["hash"] != "ABCDEF" || payload["size"] != "123" || payload["upload_type"] != "UPLOAD_TYPE_RESUMABLE" {
			t.Error(payload)
		}
		io.WriteString(w, `{"file":{"id":"remote","parent_id":"target","name":"video.mp4","kind":"drive#file","size":"123","hash":"ABCDEF","phase":"PHASE_TYPE_RUNNING"},"resumable":{"params":{"access_key_id":"key"}}}`)
	}))
	defer server.Close()
	c := New(Credentials{AccessToken: "access", CaptchaToken: "captcha"})
	c.DriveURL = server.URL
	ticket, e := c.BeginUpload(context.Background(), "target", File{Name: "video.mp4", Size: 123, Hash: "abcdef"})
	if e != nil || ticket.File == nil || ticket.File.ID != "remote" {
		t.Fatal(ticket, e)
	}
}

func TestGCIDExactContentAndTruncation(t *testing.T) {
	first := bytes.Repeat([]byte{1}, 256<<10)
	second := []byte("last block")
	data := append(first, second...)
	a, b := sha1.Sum(first), sha1.Sum(second)
	sum := sha1.Sum(append(a[:], b[:]...))
	want := strings.ToUpper(hex.EncodeToString(sum[:]))
	got, e := GCID(bytes.NewReader(data), int64(len(data)))
	if e != nil || got != want {
		t.Fatal(got, want, e)
	}
	for _, size := range []int64{int64(len(data) - 1), int64(len(data) + 1)} {
		if _, e = GCID(bytes.NewReader(data), size); e == nil {
			t.Fatal("wrong source size accepted")
		}
	}
	if _, e = GCID(strings.NewReader(""), 0); e != nil {
		t.Fatal(e)
	}
}

type uploadRoundTrip func(*http.Request) (*http.Response, error)

func (f uploadRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestMultipartResumesLostResponseWithoutReupload(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), (9<<20)/16)
	parts := map[string][]byte{}
	puts := map[string]int{}
	lost := true
	committed := false
	creates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Security-Token") != "session-token" || !strings.Contains(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
			t.Error("unsigned storage request")
		}
		q := r.URL.Query()
		switch {
		case r.Method == "POST" && q.Has("uploads"):
			creates++
			io.WriteString(w, `<InitiateMultipartUploadResult><UploadId>upload-1</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == "GET":
			io.WriteString(w, `<ListPartsResult><IsTruncated>false</IsTruncated>`)
			for n, b := range parts {
				fmt.Fprintf(w, `<Part><PartNumber>%s</PartNumber><ETag>etag-%s</ETag><Size>%d</Size></Part>`, n, n, len(b))
			}
			io.WriteString(w, `</ListPartsResult>`)
		case r.Method == "PUT":
			n := q.Get("partNumber")
			puts[n]++
			b, _ := io.ReadAll(r.Body)
			parts[n] = b
			if lost {
				lost = false
				http.Error(w, `<Error><Code>InternalError</Code><Message>secret-value</Message></Error>`, 500)
				return
			}
			w.Header().Set("ETag", "etag-"+n)
		case r.Method == "POST" && q.Has("uploadId"):
			b, _ := io.ReadAll(r.Body)
			if !bytes.Contains(b, []byte("etag-1")) || !bytes.Contains(b, []byte("etag-2")) {
				t.Error("parts missing")
			}
			committed = true
			io.WriteString(w, `<CompleteMultipartUploadResult><ETag>complete</ETag></CompleteMultipartUploadResult>`)
		default:
			t.Error(r.Method, r.URL.String())
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	c := New(Credentials{})
	target := server.Listener.Addr().String()
	c.HTTP = &http.Client{Transport: uploadRoundTrip(func(r *http.Request) (*http.Response, error) {
		r.URL.Scheme = "http"
		r.URL.Host = target
		r.Host = target
		return http.DefaultTransport.RoundTrip(r)
	})}
	u := UploadSession{}
	u.Ticket.Resumable = &struct {
		Params UploadParams `json:"params"`
	}{UploadParams{AccessKeyID: "access-key", AccessKeySecret: "private-key", SecurityToken: "session-token", Bucket: "bucket", Key: "key"}}
	read := func(_ context.Context, off, n int64) ([]byte, error) { return data[off : off+n], nil }
	saves := 0
	save := func(int64) error { saves++; return nil }
	e := c.ContinueUpload(context.Background(), &u, int64(len(data)), read, save)
	if e == nil || strings.Contains(e.Error(), "secret-value") {
		t.Fatal(e)
	}
	if u.UploadID != "upload-1" {
		t.Fatal("session not saved")
	}
	if e = c.ContinueUpload(context.Background(), &u, int64(len(data)), read, save); e != nil {
		t.Fatal(e)
	}
	if !u.Sent || !committed || creates != 1 || puts["1"] != 1 || puts["2"] != 1 || saves < 4 {
		t.Fatal(u, creates, puts, saves)
	}
	if !bytes.Equal(append(parts["1"], parts["2"]...), data) {
		t.Fatal("corrupt upload")
	}
}
