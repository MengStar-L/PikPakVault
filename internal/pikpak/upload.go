package pikpak

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

type UploadParams struct {
	AccessKeyID     string `json:"access_key_id"`
	AccessKeySecret string `json:"access_key_secret"`
	SecurityToken   string `json:"security_token"`
	Bucket          string `json:"bucket"`
	Key             string `json:"key"`
	Expiration      string `json:"expiration"`
}
type UploadTicket struct {
	File      *File `json:"file"`
	Resumable *struct {
		Params UploadParams `json:"params"`
	} `json:"resumable"`
}

// UploadSession contains temporary credentials and must be encrypted at rest.
type UploadSession struct {
	Ticket     UploadTicket     `json:"ticket"`
	UploadID   string           `json:"upload_id"`
	Creating   bool             `json:"creating"`
	Committing bool             `json:"committing"`
	Sent       bool             `json:"sent"`
	PartSize   int64            `json:"part_size"`
	Parts      map[int32]string `json:"parts"`
}
type RangeReader func(context.Context, int64, int64) ([]byte, error)
type UploadProvider interface {
	BeginUpload(context.Context, string, File) (UploadTicket, error)
	ContinueUpload(context.Context, *UploadSession, int64, RangeReader, func(int64) error) error
}

// GCID is SHA1 over the SHA1s of size-dependent blocks (rclone protocol
// reference, MIT). It is unrelated to both magnet BTIH and TelDrive BLAKE3.
func GCID(r io.Reader, size int64) (string, error) {
	if size < 0 {
		return "", fmt.Errorf("negative file size")
	}
	block := int64(256 << 10)
	for size > block*512 && block < 2<<20 {
		block *= 2
	}
	total := sha1.New()
	part := sha1.New()
	for remaining := size; remaining > 0; {
		n := min(block, remaining)
		part.Reset()
		if _, e := io.CopyN(part, r, n); e != nil {
			return "", e
		}
		total.Write(part.Sum(nil))
		remaining -= n
	}
	var extra [1]byte
	if n, e := io.ReadFull(r, extra[:]); n != 0 || e != io.EOF {
		return "", fmt.Errorf("source length differs from metadata")
	}
	return strings.ToUpper(hex.EncodeToString(total.Sum(nil))), nil
}
func (c *Client) BeginUpload(ctx context.Context, parent string, f File) (UploadTicket, error) {
	var t UploadTicket
	e := c.drive(ctx, "POST", "/drive/v1/files", nil, map[string]any{"kind": "drive#file", "name": f.Name, "parent_id": parent, "folder_type": "NORMAL", "size": strconv.FormatInt(int64(f.Size), 10), "hash": strings.ToUpper(f.Hash), "upload_type": "UPLOAD_TYPE_RESUMABLE", "resumable": map[string]string{"provider": "PROVIDER_ALIYUN"}}, &t)
	if e == nil && (t.File == nil || t.File.ID == "" || (!t.File.Complete() && t.Resumable == nil)) {
		e = &APIError{Status: 502, Code: "invalid_upload_ticket", Message: "上传凭证或文件 ID 缺失"}
	}
	return t, e
}

type guardedTransport struct {
	base  http.RoundTripper
	guard func() (func(), error)
}

func (t guardedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.guard != nil {
		unlock, e := t.guard()
		if e != nil {
			return nil, e
		}
		defer unlock()
	}
	return t.base.RoundTrip(r)
}
func storageError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	status := 0
	code := "storage_request_failed"
	var response *smithyhttp.ResponseError
	if errors.As(err, &response) {
		status = response.HTTPStatusCode()
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		code = api.ErrorCode()
	}
	// Never log signed URLs, keys or arbitrary storage error messages.
	return &APIError{Status: status, Endpoint: "upload/storage", Code: code, Message: "分片上传请求失败；重试时先核对已上传分片"}
}
func (c *Client) ContinueUpload(ctx context.Context, u *UploadSession, size int64, read RangeReader, save func(int64) error) error {
	if u.Sent {
		return nil
	}
	if u.Ticket.Resumable == nil {
		return fmt.Errorf("PikPak 没有返回可用上传凭证")
	}
	p := u.Ticket.Resumable.Params
	if p.AccessKeyID == "" || p.AccessKeySecret == "" || p.Bucket == "" || p.Key == "" {
		return fmt.Errorf("PikPak 上传凭证不完整")
	}
	if expiry, e := time.Parse(time.RFC3339, p.Expiration); e == nil && time.Now().After(expiry) {
		return fmt.Errorf("PikPak 上传凭证已过期；请核对远端未完成文件后重新同步，程序不会反复创建文件")
	}
	transport := c.HTTP.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	httpClient := &http.Client{Timeout: 3 * time.Minute, Transport: guardedTransport{transport, c.BeforeWrite}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	client := s3.New(s3.Options{Region: "pikpak", BaseEndpoint: aws.String("https://mypikpak.com"), Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: p.AccessKeyID, SecretAccessKey: p.AccessKeySecret, SessionToken: p.SecurityToken}, nil
	}), HTTPClient: httpClient, RetryMaxAttempts: 1, RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired, ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired})
	if u.PartSize == 0 {
		u.PartSize = max(8<<20, ((size+9999)/10000+(1<<20)-1)/(1<<20)*(1<<20))
	}
	if u.PartSize > 64<<20 {
		return fmt.Errorf("文件超过当前分片上传上限（625 GiB）")
	}
	if size <= u.PartSize {
		b, e := read(ctx, 0, size)
		if e != nil {
			return e
		}
		if int64(len(b)) != size {
			return io.ErrUnexpectedEOF
		}
		if e = save(0); e != nil {
			return e
		}
		_, e = client.PutObject(ctx, &s3.PutObjectInput{Bucket: &p.Bucket, Key: &p.Key, ContentLength: aws.Int64(size), Body: bytes.NewReader(b)})
		if e != nil {
			return storageError(e)
		}
		u.Sent = true
		return save(size)
	}
	if u.UploadID == "" {
		if u.Creating {
			return fmt.Errorf("分片会话创建响应丢失，已暂停以避免重复创建；请核对远端上传")
		}
		u.Creating = true
		if e := save(0); e != nil {
			return e
		}
		r, e := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &p.Bucket, Key: &p.Key})
		if e != nil {
			return storageError(e)
		}
		if aws.ToString(r.UploadId) == "" {
			return fmt.Errorf("PikPak 分片会话 ID 缺失")
		}
		u.UploadID = *r.UploadId
		u.Creating = false
		if e = save(0); e != nil {
			return e
		}
	}
	// List every remote part on each resume, including after a lost part response.
	u.Parts = map[int32]string{}
	var marker *string
	markers := map[string]bool{}
	uploaded := int64(0)
	for {
		r, e := client.ListParts(ctx, &s3.ListPartsInput{Bucket: &p.Bucket, Key: &p.Key, UploadId: &u.UploadID, PartNumberMarker: marker})
		if e != nil {
			return storageError(e)
		}
		for _, part := range r.Parts {
			n := aws.ToInt32(part.PartNumber)
			expected := min(u.PartSize, size-int64(n-1)*u.PartSize)
			if n <= 0 || expected <= 0 || aws.ToInt64(part.Size) != expected || aws.ToString(part.ETag) == "" {
				return fmt.Errorf("远端分片大小或序号不匹配")
			}
			u.Parts[n] = *part.ETag
		}
		if !aws.ToBool(r.IsTruncated) {
			break
		}
		m := aws.ToString(r.NextPartNumberMarker)
		if m == "" || markers[m] {
			return fmt.Errorf("远端分片分页不完整")
		}
		markers[m] = true
		marker = &m
	}
	parts := []types.CompletedPart{}
	for offset, number := int64(0), int32(1); offset < size; number++ {
		length := min(u.PartSize, size-offset)
		etag := u.Parts[number]
		if etag == "" {
			if e := save(uploaded); e != nil {
				return e
			}
			b, e := read(ctx, offset, length)
			if e != nil {
				return e
			}
			if int64(len(b)) != length {
				return io.ErrUnexpectedEOF
			}
			r, e := client.UploadPart(ctx, &s3.UploadPartInput{Bucket: &p.Bucket, Key: &p.Key, UploadId: &u.UploadID, PartNumber: aws.Int32(number), ContentLength: aws.Int64(length), Body: bytes.NewReader(b)})
			if e != nil {
				return storageError(e)
			}
			etag = aws.ToString(r.ETag)
			if etag == "" {
				return fmt.Errorf("上传分片未返回 ETag")
			}
			u.Parts[number] = etag
		}
		parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(number), ETag: aws.String(etag)})
		uploaded += length
		offset += length
		if e := save(uploaded); e != nil {
			return e
		}
	}
	u.Committing = true
	if e := save(uploaded); e != nil {
		return e
	}
	_, e := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: &p.Bucket, Key: &p.Key, UploadId: &u.UploadID, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
	if e != nil {
		return storageError(e)
	}
	u.Sent = true
	return save(size)
}
