package dto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/common"
)

// Public single-image task API (§6.1), authoritative schema frozen by WIS-543
// RFC §3.1 and Krislliu R3 ruling 2026-07-16 (product-compatible minimal field
// set B; edit input-image cap frozen at 8). This is a deliberately NARROW
// contract: the only generation parameters are model/prompt/quality/size, and
// edit adds an ordered images[] array. Every other OpenAI-compatible field —
// response_format, style, user, background, moderation, output_format,
// output_compression, watermark, n, and the legacy multipart image/mask parts
// — is unknown and rejected with 400 INVALID_REQUEST. Fields open additively
// only when a real caller and provider-capability evidence exist.
//
// Unlike relay request DTOs (dto/openai_image.go), these structs are decoded
// from client JSON and consumed by the image-task processor to build a fresh
// upstream call — not re-marshaled verbatim — so Rule 6 pointer-preservation
// does not apply to upstream fidelity. Pointers are used here only to let
// validation distinguish an absent optional field from an explicit empty one.

const ImageTaskObjectIdentifier = "image.task"

// MinIdempotencyKeyLen / MaxIdempotencyKeyLen bound the §6.1 Idempotency-Key
// length in bytes; the upper bound matches the idempotency_key varchar(191).
const (
	MinIdempotencyKeyLen = 1
	MaxIdempotencyKeyLen = 191
)

// MaxImageTaskInputs is the §6.1 frozen cap on edit input images. More than
// this is a client contract violation rejected with 400, never truncated.
const MaxImageTaskInputs = 8

// maxImageURLBytes is a defensive upper bound on a single image_url length.
// §6.1 specifies only "non-empty absolute https URL"; this bound guards against
// abuse and is an implementation defense, not a contract value.
const maxImageURLBytes = 8192

const imageTaskJSONMediaType = "application/json"

// ImageTaskPublicStatus is the set of status values exposed on the public task
// object. The richer submission_unknown lifetime is internal to the execution
// row and never leaves the public API, which maps it onto in_progress.
type ImageTaskPublicStatus string

const (
	ImageTaskStatusQueued          ImageTaskPublicStatus = "queued"
	ImageTaskStatusInProgress      ImageTaskPublicStatus = "in_progress"
	ImageTaskStatusCompleted       ImageTaskPublicStatus = "completed"
	ImageTaskStatusFailed          ImageTaskPublicStatus = "failed"
	ImageTaskStatusCancelRequested ImageTaskPublicStatus = "cancel_requested"
	ImageTaskStatusCancelled       ImageTaskPublicStatus = "cancelled"
	ImageTaskStatusManualReview    ImageTaskPublicStatus = "manual_review"
)

// ImageTaskResultLocator is the durable single-image result embedded in a
// completed task object.
type ImageTaskResultLocator struct {
	ContentURL string `json:"content_url"`
	MimeType   string `json:"mime_type"`
	SizeBytes  int64  `json:"size_bytes"`
	SHA256     string `json:"sha256"`
	ExpiresAt  int64  `json:"expires_at"`
}

// ImageTaskErrorBody is the machine-readable failure detail on a terminal task.
type ImageTaskErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ImageTaskObject is the resource returned by every §6.1 endpoint.
type ImageTaskObject struct {
	ID        string                  `json:"id"`
	Object    string                  `json:"object"`
	Status    ImageTaskPublicStatus   `json:"status"`
	Result    *ImageTaskResultLocator `json:"result"`
	Error     *ImageTaskErrorBody     `json:"error"`
	CreatedAt int64                   `json:"created_at"`
	UpdatedAt int64                   `json:"updated_at"`
}

// ImageTaskErrorCode is the stable machine code for a §6.1 request error. The
// handler maps each code to a fixed HTTP status.
type ImageTaskErrorCode string

const (
	ImageTaskErrInvalidRequest       ImageTaskErrorCode = "INVALID_REQUEST"
	ImageTaskErrUnsupportedMediaType ImageTaskErrorCode = "UNSUPPORTED_MEDIA_TYPE"
	ImageTaskErrUnsupportedParameter ImageTaskErrorCode = "UNSUPPORTED_PARAMETER"
	ImageTaskErrIdempotencyConflict  ImageTaskErrorCode = "IDEMPOTENCY_CONFLICT"
	ImageTaskErrTooManyRequests      ImageTaskErrorCode = "TOO_MANY_REQUESTS"
	ImageTaskErrNotFound             ImageTaskErrorCode = "NOT_FOUND"
)

// ImageTaskRequestError carries the public code and the HTTP status the handler
// must return. RetryAfter, when non-zero, is emitted as a Retry-After header.
type ImageTaskRequestError struct {
	Code       ImageTaskErrorCode
	Message    string
	StatusCode int
	RetryAfter int
}

func (e *ImageTaskRequestError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func imageTaskError(code ImageTaskErrorCode, status int, msg string) *ImageTaskRequestError {
	return &ImageTaskRequestError{Code: code, StatusCode: status, Message: msg}
}

// AsImageTaskRequestError unwraps an ImageTaskRequestError from err, returning
// nil when err is not one.
func AsImageTaskRequestError(err error) *ImageTaskRequestError {
	var target *ImageTaskRequestError
	if errors.As(err, &target) {
		return target
	}
	return nil
}

// ValidateImageTaskContentType enforces the §6.1 rule that both create routes
// use application/json (charset permitted). Any other media type — including
// multipart and a missing Content-Type — yields 415 UNSUPPORTED_MEDIA_TYPE.
func ValidateImageTaskContentType(contentType string) error {
	media, _, err := mime.ParseMediaType(contentType)
	if err != nil || media != imageTaskJSONMediaType {
		return imageTaskError(ImageTaskErrUnsupportedMediaType, 415, "Content-Type must be application/json")
	}
	return nil
}

// ImageTaskGenerationRequest is the POST /v1/image-tasks/generations body. The
// field set is the frozen §6.1 generation surface; quality and size are
// pointers so an explicit empty string is distinguishable from absence (§6.1:
// "出现时必须非空").
type ImageTaskGenerationRequest struct {
	Model   string  `json:"model"`
	Prompt  string  `json:"prompt"`
	Quality *string `json:"quality,omitempty"`
	Size    *string `json:"size,omitempty"`
}

// DecodeImageTaskGenerationRequest strictly decodes a generation request body.
// Any unknown field, duplicate object key at any level, explicit n, malformed
// JSON, or non-object top level yields 400 INVALID_REQUEST.
func DecodeImageTaskGenerationRequest(body []byte) (ImageTaskGenerationRequest, error) {
	var req ImageTaskGenerationRequest
	if err := strictDecodeImageTaskBody(body, &req); err != nil {
		return req, err
	}
	if err := req.validate(); err != nil {
		return req, err
	}
	return req, nil
}

func (req ImageTaskGenerationRequest) validate() error {
	return validateGenerationFields(req.Model, req.Prompt, req.Quality, req.Size)
}

// validateGenerationFields checks the shared generation surface (model/prompt
// required and trimmed-non-empty; quality/size optional but non-empty when
// present). edit reuses it because §6.1 defines edit as the generation field
// set plus images[].
func validateGenerationFields(model, prompt string, quality, size *string) error {
	if strings.TrimSpace(model) == "" {
		return imageTaskError(ImageTaskErrInvalidRequest, 400, "model is required")
	}
	if strings.TrimSpace(prompt) == "" {
		return imageTaskError(ImageTaskErrInvalidRequest, 400, "prompt is required")
	}
	if err := validateOptionalNonEmpty("quality", quality); err != nil {
		return err
	}
	return validateOptionalNonEmpty("size", size)
}

// validateOptionalNonEmpty enforces "出现时必须非空" for a pointer optional
// field: nil is absent (ok); a non-nil trimmed-empty value is rejected.
func validateOptionalNonEmpty(name string, v *string) error {
	if v == nil {
		return nil
	}
	if strings.TrimSpace(*v) == "" {
		return imageTaskError(ImageTaskErrInvalidRequest, 400, fmt.Sprintf("%s must not be empty", name))
	}
	return nil
}

// ValidateIdempotencyKey enforces the §6.1 1..191 byte bound on Idempotency-Key.
func ValidateIdempotencyKey(key string) error {
	if len(key) < MinIdempotencyKeyLen {
		return imageTaskError(ImageTaskErrInvalidRequest, 400, "Idempotency-Key is required")
	}
	if len(key) > MaxIdempotencyKeyLen {
		return imageTaskError(ImageTaskErrInvalidRequest, 400, fmt.Sprintf("Idempotency-Key exceeds %d bytes", MaxIdempotencyKeyLen))
	}
	return nil
}

// strictDecodeImageTaskBody runs the two §6.1 strict checks the standard
// encoding/json decoder cannot: duplicate object keys at any level (handled
// here by a token walk), then DisallowUnknownFields + type correctness via
// common.UnmarshalStrict. A non-object top level is rejected by both passes.
func strictDecodeImageTaskBody(body []byte, target any) error {
	if len(body) == 0 {
		return imageTaskError(ImageTaskErrInvalidRequest, 400, "request body is required")
	}
	if err := detectDuplicateJSONKeys(body); err != nil {
		return err
	}
	if err := common.UnmarshalStrict(body, target); err != nil {
		return imageTaskError(ImageTaskErrInvalidRequest, 400, normalizeStrictDecodeError(err))
	}
	return nil
}

// normalizeStrictDecodeError collapses decoder error variants into a stable
// message so parser wording never reaches the public API.
func normalizeStrictDecodeError(err error) string {
	msg := err.Error()
	if trimmed := strings.TrimPrefix(msg, "json: unknown field "); trimmed != msg {
		return fmt.Sprintf("unknown field %s", trimmed)
	}
	return "malformed request body"
}

// detectDuplicateJSONKeys walks the JSON token stream and rejects a duplicate
// key at any object nesting level, as well as a non-object top level and any
// structural malformedness. encoding/json otherwise silently keeps the last
// value for a repeated key, which §6.1 forbids.
func detectDuplicateJSONKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return imageTaskError(ImageTaskErrInvalidRequest, 400, "malformed request body")
	}
	open, ok := tok.(json.Delim)
	if !ok || open != '{' {
		return imageTaskError(ImageTaskErrInvalidRequest, 400, "request body must be a JSON object")
	}
	var stack []*keyScope
	stack = append(stack, &keyScope{isObject: true, seen: make(map[string]struct{})})
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			if len(stack) != 0 {
				return imageTaskError(ImageTaskErrInvalidRequest, 400, "malformed request body")
			}
			return nil
		}
		if err != nil {
			return imageTaskError(ImageTaskErrInvalidRequest, 400, "malformed request body")
		}
		if len(stack) == 0 {
			// Top-level object already closed; any further token is trailing data.
			return imageTaskError(ImageTaskErrInvalidRequest, 400, "malformed request body")
		}
		top := stack[len(stack)-1]
		if delim, isDelim := tok.(json.Delim); isDelim {
			switch delim {
			case '{', '[':
				scope := &keyScope{isObject: delim == '{'}
				if scope.isObject {
					scope.seen = make(map[string]struct{})
				}
				stack = append(stack, scope)
				if top.isObject {
					top.awaitingValue = false
				}
			case '}', ']':
				stack = stack[:len(stack)-1]
			}
			continue
		}
		if top.isObject && !top.awaitingValue {
			key, ok := tok.(string)
			if !ok {
				return imageTaskError(ImageTaskErrInvalidRequest, 400, "malformed request body")
			}
			if _, dup := top.seen[key]; dup {
				return imageTaskError(ImageTaskErrInvalidRequest, 400, fmt.Sprintf("duplicate field %q", key))
			}
			top.seen[key] = struct{}{}
			top.awaitingValue = true
			continue
		}
		// scalar value token
		if top.isObject {
			top.awaitingValue = false
		}
	}
}

type keyScope struct {
	isObject      bool
	seen          map[string]struct{}
	awaitingValue bool
}

// validateImageURL enforces the §6.1 image_url constraint: non-empty, absolute
// https URL, within the defensive length bound.
func validateImageURL(raw string) error {
	if raw == "" {
		return imageTaskError(ImageTaskErrInvalidRequest, 400, "images[].image_url is required")
	}
	if len(raw) > maxImageURLBytes {
		return imageTaskError(ImageTaskErrInvalidRequest, 400, fmt.Sprintf("images[].image_url exceeds %d bytes", maxImageURLBytes))
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Scheme != "https" {
		return imageTaskError(ImageTaskErrInvalidRequest, 400, "images[].image_url must be an absolute https URL")
	}
	return nil
}
