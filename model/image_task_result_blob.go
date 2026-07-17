package model

import (
	"errors"

	"gorm.io/gorm"
)

// ImageTaskResultBlob is the durable persistence of one completed image task's
// result bytes (§7.6). ApiNebula returns a temporary upstream download_url that
// expires; the processor downloads the bytes, validates them, and stores them
// here so the result survives the upstream URL expiry. The blob is keyed by
// execution_id (one durable result per execution); the large content column
// lives in its own table so the Task/execution JSON never carries image bytes.
//
// The content column is a LONGBLOB on MySQL and a BLOB on SQLite/PostgreSQL via
// GORM's type:bytes tag. The table is additive (P3); existing image task tables
// are unchanged.
type ImageTaskResultBlob struct {
	ID          int64  `json:"id" gorm:"primary_key;AUTO_INCREMENT"`
	ExecutionID int64  `json:"execution_id" gorm:"uniqueIndex"`
	PublicTaskID string `json:"public_task_id" gorm:"type:varchar(64);index"`
	MimeType    string `json:"mime_type" gorm:"type:varchar(100)"`
	SizeBytes   int64  `json:"size_bytes"`
	SHA256      string `json:"sha256" gorm:"type:varchar(64);index"`
	Content     []byte `json:"-" gorm:"type:bytes"`
	ExpiresAt   int64  `json:"expires_at" gorm:"index"`
	CreatedAt   int64  `json:"created_at" gorm:"index"`
}

// ErrImageTaskResultBlobConflict is returned when a blob already exists for an
// execution but with a different sha256 than the caller computed. It signals a
// non-idempotent re-download (the upstream served different bytes) and the
// processor treats it as a result_store_error rather than silently overwriting.
var ErrImageTaskResultBlobConflict = errors.New("image task result blob already exists with different content")

// GetImageTaskResultBlob loads the durable result blob for one execution. A
// missing row is reported as gorm.ErrRecordNotFound.
func GetImageTaskResultBlob(executionID int64) (*ImageTaskResultBlob, error) {
	var blob ImageTaskResultBlob
	if err := DB.Where("execution_id = ?", executionID).First(&blob).Error; err != nil {
		return nil, err
	}
	return &blob, nil
}

// CreateImageTaskResultBlob inserts a new result blob. It relies on the
// execution_id unique index as the idempotency fence: a concurrent or retried
// store that races loses on the unique constraint, and the caller re-reads the
// winner. A duplicate with the same sha256 is a benign idempotent replay; a
// duplicate with a different sha256 is ErrImageTaskResultBlobConflict.
func CreateImageTaskResultBlob(blob *ImageTaskResultBlob) (created bool, stored *ImageTaskResultBlob, err error) {
	if blob == nil {
		return false, nil, errors.New("create image task result blob: nil blob")
	}
	txErr := DB.Transaction(func(tx *gorm.DB) error {
		var existing ImageTaskResultBlob
		lookup := tx.Where("execution_id = ?", blob.ExecutionID).First(&existing)
		if lookup.Error == nil {
			stored = &existing
			if existing.SHA256 != blob.SHA256 {
				return ErrImageTaskResultBlobConflict
			}
			created = false
			return nil
		}
		if !errors.Is(lookup.Error, gorm.ErrRecordNotFound) {
			return lookup.Error
		}
		if err := tx.Create(blob).Error; err != nil {
			return err
		}
		created = true
		stored = blob
		return nil
	})
	if txErr != nil {
		return false, nil, txErr
	}
	return created, stored, nil
}
