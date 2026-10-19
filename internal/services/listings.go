package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tecisage/resell-apis/internal/dtos"
	"github.com/tecisage/resell-apis/internal/jobs"
	"github.com/tecisage/resell-apis/internal/storage"
)

var ErrListingNotFound = errors.New("listing not found")

type ListingService struct {
	Logger  *slog.Logger
	Db      *sql.DB
	Storage *storage.Client
}

func NewListingService(logger *slog.Logger, db *sql.DB, storage *storage.Client) *ListingService {
	return &ListingService{
		Logger:  logger,
		Db:      db,
		Storage: storage,
	}
}

func (ls *ListingService) CreateListing(ctx context.Context, reqId string, req dtos.CreateListingRequest, userId string, listingUploadKeys dtos.ListingUploadKeys) (dtos.CreateListingResponse, error) {
	// 1. Verify uploaded images with R2
	for _, uk := range listingUploadKeys.ImageObjectKeys {
		_, _, err := ls.Storage.HeadObject(ctx, uk.OriginalKey)
		if err != nil {
			switch {
			case errors.Is(err, storage.ErrNotFound):
				return dtos.CreateListingResponse{}, &dtos.ValidationError{Field: "image_keys", Message: fmt.Sprintf("image not found: %s", uk.OriginalKey)}
			case errors.Is(err, storage.ErrZeroSizeObject):
				return dtos.CreateListingResponse{}, &dtos.ValidationError{Field: "image_keys", Message: "image has zero content length"}
			case errors.Is(err, storage.ErrFileSizeExceeded):
				return dtos.CreateListingResponse{}, &dtos.ValidationError{Field: "image_keys", Message: "image size exceeds limit"}
			case errors.Is(err, storage.ErrNoContentType):
				return dtos.CreateListingResponse{}, &dtos.ValidationError{Field: "image_keys", Message: "image has no content type"}
			case errors.Is(err, storage.ErrInvalidContentType):
				return dtos.CreateListingResponse{}, &dtos.ValidationError{Field: "image_keys", Message: "uploaded object has an unsupported content type"}
			default:
				ls.Logger.Error("Listing Service: failed to head object", "key", uk.OriginalKey, "request_id", reqId, "err", err)
				return dtos.CreateListingResponse{}, err
			}
		}
	}

	// 2. Start DB Transaction
	tx, err := ls.Db.BeginTx(ctx, nil)
	if err != nil {
		ls.Logger.Error("Listing Service: failed to begin transaction", "request_id", reqId, "err", err)
		return dtos.CreateListingResponse{}, err
	}
	defer tx.Rollback()

	var primaryImage *string
	if len(listingUploadKeys.ImageObjectKeys) > 0 {
		primaryImage = &listingUploadKeys.ImageObjectKeys[0].FinalObjectKey
	}

	query := `
		INSERT INTO listings (id, title, description, price, city, image, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, title, status, created_at;
	`

	priceInPaise := dtos.RupeesToPaise(req.Price)

	// Insert listing into database
	var out dtos.CreateListingResponse
	err = tx.QueryRowContext(ctx, query, listingUploadKeys.ListingID, req.Title, req.Description, priceInPaise, req.City, primaryImage, userId).
		Scan(&out.ID, &out.Title, &out.Status, &out.Created)
	if err != nil {
		ls.Logger.Error("Listing Service: CreateListing insert error", "request_id", reqId, "err", err)
		return dtos.CreateListingResponse{}, err
	}

	// Insert images into database
	for i, s := range listingUploadKeys.ImageObjectKeys {
		query := `
			INSERT INTO images (listing_id, object_key, position)
			VALUES ($1, $2, $3);
		`

		imageUUID := uuid.MustParse(s.ImageID)
		if _, err := tx.ExecContext(ctx, query, listingUploadKeys.ListingID, dtos.MintImageKey(imageUUID), int16(i)); err != nil {
			ls.Logger.Error("Listing Service: CreateListing insert image error", "request_id", reqId, "err", err)
			return dtos.CreateListingResponse{}, err
		}
	}

	// Create job in jobs table for image processing
	payload, err := json.Marshal(ls.prepareJobPayload(listingUploadKeys))
	if err != nil {
		ls.Logger.Error("Listing Service: failed to marshal payload", "request_id", reqId, "err", err)
		return dtos.CreateListingResponse{}, err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO jobs (kind, payload)
		VALUES ($1, $2);
	`, jobs.KindProcessingListingImages, payload)
	if err != nil {
		ls.Logger.Error("Listing Service: CreateListing insert job error", "request_id", reqId, "err", err)
		return dtos.CreateListingResponse{}, err
	}

	// 3. Commit DB Transaction
	if err := tx.Commit(); err != nil {
		ls.Logger.Error("Listing Service: failed to commit transaction", "request_id", reqId, "err", err)
		return dtos.CreateListingResponse{}, err
	}

	ls.Logger.Info("Listing created successfully", "id", out.ID, "request_id", reqId)
	return out, nil
}

func (ls *ListingService) DeleteListing(ctx context.Context, reqId string, listingId string, userId string) (int64, error) {
	result, err := ls.Db.ExecContext(ctx, `DELETE FROM listings WHERE id = $1 AND created_by = $2`, listingId, userId)
	if err != nil {
		ls.Logger.Error("Delete Listing Service: DeleteListing exec error", "listing_id", listingId, "request_id", reqId, "err", err)
		return -1, err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		ls.Logger.Error("Listing Service: DeleteListing rowsAffected error", "listing_id", listingId, "request_id", reqId, "err", err)
		return -1, err
	}

	return rowsAffected, nil
}

func (ls *ListingService) GetListings(ctx context.Context, reqId string, limit int, cursor *dtos.ListingCursor, filter dtos.ListingFilter) (*dtos.PaginatedListingsResponse, error) {
	fetchLimit := limit + 1
	query, args := ls.buildGetListingsQuery(fetchLimit, cursor, filter)

	rows, err := ls.Db.QueryContext(ctx, query, args...)
	if err != nil {
		ls.Logger.Error("Listing Service: GetListings query error", "request_id", reqId, "err", err)
		return nil, err
	}
	defer rows.Close()

	listings, err := scanListings(rows)
	if err != nil {
		ls.Logger.Error("Listing Service: GetListings scan error", "request_id", reqId, "err", err)
		return nil, err
	}

	hasMore := len(listings) > limit
	if hasMore {
		listings = listings[:limit]
	}

	var nextCursor *string
	if hasMore && len(listings) > 0 {
		lastItem := listings[len(listings)-1]
		encoded, err := dtos.EncodeCursor(lastItem.ID, lastItem.CreatedAt)
		if err != nil {
			ls.Logger.Error("Listing Service: GetListings encode cursor error", "request_id", reqId, "err", err)
			return nil, err
		}
		nextCursor = &encoded
	}

	return &dtos.PaginatedListingsResponse{
		Data: listings,
		Pagination: dtos.PaginationMeta{
			Limit:      limit,
			NextCursor: nextCursor,
			HasMore:    hasMore,
		},
	}, nil
}

func (ls *ListingService) GetListingByID(ctx context.Context, reqId string, listingId string) (*dtos.ListingResponse, error) {
	query := `
		SELECT 
			l.id, l.title, l.description, l.price, l.city, l.image, l.status, l.created_at, l.updated_at, l.created_by,
			i.id as image_id, i.object_key, i.position
		FROM listings l
		LEFT JOIN images i ON i.listing_id = l.id
		WHERE l.id = $1
		ORDER BY i.position ASC;
	`

	rows, err := ls.Db.QueryContext(ctx, query, listingId)
	if err != nil {
		ls.Logger.Error("Listing Service: GetListingByID query error", "request_id", reqId, "listing_id", listingId, "err", err)
		return nil, err
	}
	defer rows.Close()

	listings, err := scanListings(rows)
	if err != nil {
		ls.Logger.Error("Listing Service: GetListingByID scan error", "request_id", reqId, "listing_id", listingId, "err", err)
		return nil, err
	}

	if len(listings) == 0 {
		return nil, ErrListingNotFound
	}

	return &listings[0], nil
}

func (ls *ListingService) buildGetListingsQuery(fetchLimit int, cursor *dtos.ListingCursor, filter dtos.ListingFilter) (string, []any) {
	var (
		whereClauses []string
		args         []any
		argIndex     = 1
	)

	// Status filter (default is 'ready', unless explicitly 'all')
	if filter.Status != "" && filter.Status != "all" {
		whereClauses = append(whereClauses, fmt.Sprintf("l2.status = $%d", argIndex))
		args = append(args, filter.Status)
		argIndex++
	}

	// City filter (case-insensitive)
	if filter.City != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("l2.city ILIKE $%d", argIndex))
		args = append(args, filter.City)
		argIndex++
	}

	// PriceFrom filter (client sends Rupees, DB stores Paise)
	if filter.PriceFrom != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("l2.price >= $%d", argIndex))
		args = append(args, dtos.RupeesToPaise(*filter.PriceFrom))
		argIndex++
	}

	// PriceTo filter (client sends Rupees, DB stores Paise)
	if filter.PriceTo != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("l2.price <= $%d", argIndex))
		args = append(args, dtos.RupeesToPaise(*filter.PriceTo))
		argIndex++
	}

	// Keyset cursor seek
	if cursor != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("(l2.created_at, l2.id) < ($%d, $%d)", argIndex, argIndex+1))
		args = append(args, cursor.CreatedAt, cursor.ID)
		argIndex += 2
	}

	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	limitPlaceholder := fmt.Sprintf("$%d", argIndex)
	args = append(args, fetchLimit)

	query := fmt.Sprintf(`
		SELECT 
			l.id, l.title, l.description, l.price, l.city, l.image, l.status, l.created_at, l.updated_at, l.created_by,
			i.id as image_id, i.object_key, i.position
		FROM listings l
		LEFT JOIN images i ON i.listing_id = l.id
		WHERE l.id IN (
			SELECT id FROM listings l2
			%s
			ORDER BY l2.created_at DESC, l2.id DESC 
			LIMIT %s
		)
		ORDER BY l.created_at DESC, l.id DESC, i.position ASC;
	`, whereSQL, limitPlaceholder)

	return query, args
}

func scanListings(rows *sql.Rows) ([]dtos.ListingResponse, error) {
	listings := make([]dtos.ListingResponse, 0)
	listingIndex := make(map[string]int)

	for rows.Next() {
		var (
			id           string
			title        string
			description  string
			priceInPaise int64
			city         string
			image        *string
			status       string
			createdAt    time.Time
			updatedAt    time.Time
			createdBy    *string
			imageID      sql.NullString
			objectKey    sql.NullString
			position     sql.NullInt16
		)

		if err := rows.Scan(
			&id,
			&title,
			&description,
			&priceInPaise,
			&city,
			&image,
			&status,
			&createdAt,
			&updatedAt,
			&createdBy,
			&imageID,
			&objectKey,
			&position,
		); err != nil {
			return nil, err
		}

		idx, exists := listingIndex[id]
		if !exists {
			item := dtos.ListingResponse{
				ID:          id,
				Title:       title,
				Description: description,
				Price:       dtos.PaiseToRupees(priceInPaise),
				City:        city,
				Image:       image,
				Status:      status,
				CreatedBy:   createdBy,
				CreatedAt:   createdAt,
				UpdatedAt:   updatedAt,
				Images:      make([]dtos.ListingImage, 0),
			}
			listings = append(listings, item)
			idx = len(listings) - 1
			listingIndex[id] = idx
		}

		if imageID.Valid && objectKey.Valid {
			listings[idx].Images = append(listings[idx].Images, dtos.ListingImage{
				ID:        imageID.String,
				ObjectKey: objectKey.String,
				Position:  position.Int16,
			})
		}
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return listings, nil
}

func (ls *ListingService) prepareJobPayload(listingUploadKeys dtos.ListingUploadKeys) jobs.ProcessImagepayload {
	imageSources := make([]jobs.ImageSource, 0, len(listingUploadKeys.ImageObjectKeys))

	for _, uk := range listingUploadKeys.ImageObjectKeys {
		imageSources = append(imageSources, jobs.ImageSource{
			OriginalKey: uk.OriginalKey,
			FinalKey:    uk.FinalObjectKey,
		})
	}

	return jobs.ProcessImagepayload{
		ListingID:    listingUploadKeys.ListingID,
		ImageSources: imageSources,
	}
}
