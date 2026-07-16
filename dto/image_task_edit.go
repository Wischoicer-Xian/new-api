package dto

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/QuantumNous/new-api/common"
)

// ImageTaskEditImage is one entry of the §6.1 edit images[] array. An item
// accepts only image_url; any other field is unknown and rejected, enforced by
// the exact-key check in validateEditImagesItems.
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
// non-null absolute https image_url. Unknown fields, duplicate keys, explicit
// null, explicit n, more than 8 images, or a non-object top level all yield 400.
func DecodeImageTaskEditRequest(body []byte) (ImageTaskEditRequest, error) {
	var req ImageTaskEditRequest
	raw, err := strictDecodeImageTaskBody(body, &req, editFieldWhitelist)
	if err != nil {
		return req, err
	}
	if imagesRaw, ok := raw["images"]; ok {
		if err := validateEditImagesItems(imagesRaw); err != nil {
			return req, err
		}
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

// validateEditImagesItems enforces the §6.1 rule that each images[] entry is an
// object carrying exactly an image_url key. The raw per-item maps preserve the
// literal keys, so "Image_URL" is unknown and a null image_url is rejected here
// rather than collapsing to an empty string.
func validateEditImagesItems(imagesRaw json.RawMessage) error {
	var items []map[string]json.RawMessage
	if err := common.Unmarshal(imagesRaw, &items); err != nil {
		return imageTaskError(ImageTaskErrInvalidRequest, 400, "images must be an array of objects")
	}
	nullLiteral := []byte("null")
	for _, item := range items {
		if len(item) != 1 {
			return imageTaskError(ImageTaskErrInvalidRequest, 400, "each image must contain only image_url")
		}
		value, ok := item["image_url"]
		if !ok {
			return imageTaskError(ImageTaskErrInvalidRequest, 400, "image item must use image_url")
		}
		if bytes.Equal(bytes.TrimSpace(value), nullLiteral) {
			return imageTaskError(ImageTaskErrInvalidRequest, 400, "images[].image_url must not be null")
		}
	}
	return nil
}
