package pikpak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type Number int64

func (n *Number) UnmarshalJSON(b []byte) error {
	if string(b) == "null" || string(b) == `""` {
		return nil
	}
	v, err := strconv.ParseInt(strings.Trim(string(b), `"`), 10, 64)
	*n = Number(v)
	return err
}

type File struct {
	ID             string          `json:"id"`
	ParentID       string          `json:"parent_id"`
	Name           string          `json:"name"`
	Kind           string          `json:"kind"`
	Size           Number          `json:"size"`
	Hash           string          `json:"hash"`
	MimeType       string          `json:"mime_type"`
	Phase          string          `json:"phase"`
	Trashed        bool            `json:"trashed"`
	Thumbnail      string          `json:"thumbnail_link"`
	OriginalURL    string          `json:"original_url"`
	WebContentLink string          `json:"web_content_link"`
	Medias         []Media         `json:"medias"`
	Links          map[string]Link `json:"links"`
}

func (f File) Folder() bool   { return f.Kind == "drive#folder" }
func (f File) Complete() bool { return f.Folder() || f.Phase == "PHASE_TYPE_COMPLETE" }

type Link struct {
	URL    string `json:"url"`
	Expire string `json:"expire"`
}
type Media struct {
	ID         string `json:"media_id"`
	Name       string `json:"media_name"`
	Resolution string `json:"resolution_name"`
	Link       Link   `json:"link"`
	Original   bool   `json:"is_origin"`
}
type Page struct {
	Files []File `json:"files"`
	Next  string `json:"next_page_token"`
}
type Task struct {
	ID       string     `json:"id"`
	FileID   string     `json:"file_id"`
	Phase    string     `json:"phase"`
	Progress int        `json:"progress"`
	Message  string     `json:"message"`
	Params   TaskParams `json:"params"`
}
type TaskParams struct {
	URL          string          `json:"url"`
	TraceFileIDs json.RawMessage `json:"trace_file_ids,omitempty"`
	ErrorDetail  string          `json:"error_detail,omitempty"`
}
type Transfer struct {
	File    *File    `json:"file"`
	Task    *Task    `json:"task"`
	Files   []File   `json:"files"`
	FileIDs []string `json:"file_ids"`
	TaskID  string   `json:"task_id"`
	// Share restore file_id identifies the destination folder, never an output.
	RestoreParentID string     `json:"file_id,omitempty"`
	RestoreTaskID   string     `json:"restore_task_id,omitempty"`
	RestoreStatus   string     `json:"restore_status,omitempty"`
	Params          TaskParams `json:"params"`
}

type TaskReader interface {
	Task(context.Context, string) (Task, error)
}
type Share struct {
	Page
	PassCodeToken string `json:"pass_code_token"`
	Status        string `json:"share_status"`
	StatusText    string `json:"share_status_text"`
	Title         string `json:"title"`
}
type Identity struct {
	Sub   string `json:"sub"`
	Name  string `json:"name"`
	Email string `json:"email"`
}
type Quota struct {
	Limit     Number `json:"limit"`
	Usage     Number `json:"usage"`
	Unlimited bool   `json:"is_unlimited"`
}

type APIError struct {
	Status          int    `json:"status"`
	Endpoint        string `json:"endpoint"`
	Code            string `json:"code"`
	Message         string `json:"message"`
	VerificationURL string `json:"verification_url,omitempty"`
	RetryAfter      int    `json:"retry_after,omitempty"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("PikPak %s: HTTP %d (%s): %s", e.Endpoint, e.Status, e.Code, e.Message)
}
func Missing(err error) bool {
	var e *APIError
	return errors.As(err, &e) && (e.Status == 404 || e.Code == "file_not_found")
}
func Temporary(err error) bool {
	var e *APIError
	return errors.As(err, &e) && (e.Status == 429 || e.Status >= 500 || e.Status == 0)
}

type Credentials struct {
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	AccessToken   string `json:"access_token,omitempty"`
	RefreshToken  string `json:"refresh_token,omitempty"`
	DeviceID      string `json:"device_id,omitempty"`
	UserID        string `json:"user_id,omitempty"`
	CaptchaToken  string `json:"captcha_token,omitempty"`
	Expiry        int64  `json:"expiry,omitempty"`
	CaptchaExpiry int64  `json:"captcha_expiry,omitempty"`
}

// Provider keeps the state machine independent of PikPak's private HTTP API.
type Provider interface {
	Me(context.Context) (Identity, error)
	Quota(context.Context) (Quota, error)
	List(context.Context, string, string) (Page, error)
	Get(context.Context, string) (File, error)
	Mkdir(context.Context, string, string) (File, error)
	Move(context.Context, string, string) error
	Rename(context.Context, string, string) error
	Trash(context.Context, []string) error
	Untrash(context.Context, []string) error
	Offline(context.Context, string, string) (Transfer, error)
	Tasks(context.Context) ([]Task, error)
	Share(context.Context, string, string, string, string, string) (Share, error)
	RestoreShare(context.Context, string, string, []string, string) (Transfer, error)
	Instant(context.Context, string, File) (Transfer, error)
}

func DecodeError(b []byte, status int, endpoint string) *APIError {
	var body struct {
		Code      string          `json:"error"`
		Message   string          `json:"error_description"`
		ErrorCode json.RawMessage `json:"error_code"`
	}
	_ = json.Unmarshal(b, &body)
	if body.Code == "" {
		body.Code = "upstream_error"
	}
	return &APIError{Status: status, Endpoint: endpoint, Code: body.Code, Message: body.Message}
}
