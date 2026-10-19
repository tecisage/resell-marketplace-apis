package dtos

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	ListingPrefix     = "listings"
	ImageProcessedExt = ".jpg"
)

type ListingStatusType string

const (
	ListingStatusReady      ListingStatusType = "ready"
	ListingStatusProcessing ListingStatusType = "processing"
	ListingStatusRejected   ListingStatusType = "rejected"
)

type CreateListingRequest struct {
	Title       string   `json:"title" binding:"required"`
	Description string   `json:"description" binding:"required"`
	Price       int64    `json:"price" binding:"required"`
	City        string   `json:"city" binding:"required"`
	ImageKeys   []string `json:"image_keys"` //uploads/<user-id>/<image-uuid>.png
}

type ListingUploadKeys struct {
	ListingID       uuid.UUID                  `json:"listing_id"`
	ImageObjectKeys []ProcessedObjectUploadKey `json:"images"`
}

type ProcessedObjectUploadKey struct {
	OriginalKey    string // uploads/<user-id>/<image-id>.<ext> - client sent
	ImageID        string // image-id
	Extension      string // .ext
	FinalObjectKey string // listings/<listing-id>/<image-id>.<ext> - final key post image processing
}

func (req *CreateListingRequest) Validate(userId string) (ListingUploadKeys, error) {
	if strings.TrimSpace(req.Title) == "" {
		return ListingUploadKeys{}, &ValidationError{Field: "title", Message: "title is required"}
	}
	if strings.TrimSpace(req.Description) == "" {
		return ListingUploadKeys{}, &ValidationError{Field: "description", Message: "description is required"}
	}
	if req.Price <= 0 {
		return ListingUploadKeys{}, &ValidationError{Field: "price", Message: "price must be greater than 0"}
	}
	if strings.TrimSpace(req.City) == "" {
		return ListingUploadKeys{}, &ValidationError{Field: "city", Message: "city is required"}
	}

	listingUUID := uuid.New()

	parsedUploadKeys := make([]ProcessedObjectUploadKey, 0, len(req.ImageKeys))
	if req.ImageKeys != nil {
		for _, key := range req.ImageKeys {
			parts := strings.Split(key, "/")
			if len(parts) != 3 {
				return ListingUploadKeys{}, &ValidationError{Field: "image_keys", Message: "invalid image key format"}
			}

			prefix := parts[0]
			userIDInKey := parts[1]
			fileName := parts[2]

			// Check prefix
			if prefix != "uploads" {
				return ListingUploadKeys{}, &ValidationError{Field: "image_keys", Message: "image key must start with 'uploads/'"}
			}

			// Validate UUID format
			if _, err := uuid.Parse(userIDInKey); err != nil {
				return ListingUploadKeys{}, &ValidationError{Field: "image_keys", Message: "invalid user ID in image key"}
			}

			ext := filepath.Ext(fileName)
			imageRawUUID := strings.TrimSuffix(fileName, ext)
			if _, err := uuid.Parse(imageRawUUID); err != nil {
				return ListingUploadKeys{}, &ValidationError{Field: "image_keys", Message: "invalid file name in image key"}
			}

			// Verify key belongs to the authenticated user
			if userIDInKey != userId {
				return ListingUploadKeys{}, &ValidationError{Field: "image_keys", Message: "image key does not belong to authenticated user"}
			}

			parsedUploadKeys = append(parsedUploadKeys, ProcessedObjectUploadKey{
				OriginalKey:    key,
				ImageID:        imageRawUUID,
				Extension:      ext,
				FinalObjectKey: mintFinalObjectKey(listingUUID, uuid.MustParse(imageRawUUID)),
			})
		}
	}
	return ListingUploadKeys{ImageObjectKeys: parsedUploadKeys, ListingID: listingUUID}, nil
}

type CreateListingResponse struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Status  string `json:"status"`
	Created string `json:"created_at"`
}

type ListingImage struct {
	ID        string `json:"id"`
	ObjectKey string `json:"object_key"`
	Position  int16  `json:"position"`
}

type ListingResponse struct {
	ID          string         `json:"id"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	Price       int64          `json:"price"`
	City        string         `json:"city"`
	Image       *string        `json:"image"`
	Status      string         `json:"status"`
	CreatedBy   *string        `json:"created_by,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	Images      []ListingImage `json:"images"`
}

// {listingsPrefix}
// filepath.Ext() includes the leading dot (.jpg), formatting with "%s/%s/%s.%s" creates an extra dot:
func mintFinalObjectKey(listingID uuid.UUID, imageID uuid.UUID) string {
	return fmt.Sprintf("%s/%s/%s", ListingPrefix, listingID.String(), MintImageKey(imageID))
}

func MintImageKey(imageID uuid.UUID) string {
	// todo: currently we convert all images to .jpeg, we should use the original extension
	return fmt.Sprintf("%s%s", imageID.String(), ImageProcessedExt)
}

const CursorVersion = 1

type ListingCursor struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Version   int       `json:"version"`
}

type PaginationMeta struct {
	Limit      int     `json:"limit"`
	NextCursor *string `json:"next_cursor"`
	HasMore    bool    `json:"has_more"`
}

type PaginatedListingsResponse struct {
	Data       []ListingResponse `json:"data"`
	Pagination PaginationMeta    `json:"pagination"`
}

func EncodeCursor(id string, createdAt time.Time) (string, error) {
	payload := ListingCursor{
		ID:        id,
		CreatedAt: createdAt.UTC(),
		Version:   CursorVersion,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func DecodeCursor(encoded string) (*ListingCursor, error) {
	var b []byte
	var err error

	// Try URL safe unpadded encoding first
	b, err = base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		// Fallback to URL safe with padding
		b, err = base64.URLEncoding.DecodeString(encoded)
		if err != nil {
			// Fallback to standard encoding
			b, err = base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return nil, errors.New("invalid base64 cursor")
			}
		}
	}

	var cursor ListingCursor
	if err := json.Unmarshal(b, &cursor); err != nil {
		return nil, errors.New("invalid cursor payload")
	}

	if cursor.Version != CursorVersion {
		return nil, fmt.Errorf("unsupported cursor version: %d", cursor.Version)
	}

	if _, err := uuid.Parse(cursor.ID); err != nil {
		return nil, errors.New("invalid cursor id format")
	}

	if cursor.CreatedAt.IsZero() {
		return nil, errors.New("invalid cursor timestamp")
	}

	return &cursor, nil
}
