package dto

import (
	"fmt"
)

// ImageTaskEditImage is one entry of the §6.1 edit images[] array. An item
// accepts only image_url; any other field is unknown and rejected (enforced by
// DisallowUnknownFields, which recurses into array elements).
type ImageTaskEditImage struct {
	ImageURL string `json:"image_url"`
}

// ImageTaskEditRequest is the POST /v1/image-tasks/edits body
// (application/json). It extends the generation surface with an ordered
// images[] array of 1..8 input images. Order is semantic and participates in
// the canonical request hash; duplicate URLs are permitted.
type ImageTaskEditRequest struct {
	Model   string               `json:"model"`
	Prompt  string               `json:"prompt"`
	Quality *string              `json:"quality,omitempty"`
	Size    *string              `json:"size,omitempty"`
	Images  []ImageTaskEditImage `json:"images"`
}

// DecodeImageTaskEditRequest strictly decodes an edit request body. It enforces
// the generation field set plus the images[] array: 1..8 items, each a single
// non-empty absolute https image_url. Unknown fields, duplicate keys, explicit
// n, more than 8 images, or a non-object top level all yield 400.
func DecodeImageTaskEditRequest(body []byte) (ImageTaskEditRequest, error) {
	var req ImageTaskEditRequest
	if err := strictDecodeImageTaskBody(body, &req); err != nil {
		return req, err
	}
	if err := req.validate(); err != nil {
		return req, err
	}
	return req, nil
}

func (req ImageTaskEditRequest) validate() error {
	if err := validateGenerationFields(req.Model, req.Prompt, req.Quality, req.Size); err != nil {
		return err
	}
	if len(req.Images) < 1 {
		return imageTaskError(ImageTaskErrInvalidRequest, 400, "images must contain at least 1 item")
	}
	if len(req.Images) > MaxImageTaskInputs {
		return imageTaskError(ImageTaskErrInvalidRequest, 400, fmt.Sprintf("images must contain at most %d items", MaxImageTaskInputs))
	}
	for _, img := range req.Images {
		if err := validateImageURL(img.ImageURL); err != nil {
			return err
		}
	}
	return nil
}
