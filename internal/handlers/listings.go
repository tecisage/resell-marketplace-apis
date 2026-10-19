package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/tecisage/resell-apis/internal/dtos"
	"github.com/tecisage/resell-apis/internal/httpx"
	middleware "github.com/tecisage/resell-apis/internal/middlewares"
	"github.com/tecisage/resell-apis/internal/services"
	"github.com/tecisage/resell-apis/internal/storage"
)

const (
	DefaultListLimit = 6
	MinListLimit     = 1
	MaxListLimit     = 100
)

type ListingHandler struct {
	DB             *sql.DB
	Logger         *slog.Logger
	ListingService *services.ListingService
}

func NewListingHandler(db *sql.DB, logger *slog.Logger, storage *storage.Client) *ListingHandler {
	listingService := services.NewListingService(logger, db, storage)
	return &ListingHandler{
		DB:             db,
		Logger:         logger,
		ListingService: listingService,
	}
}

func (lh *ListingHandler) GetListings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqId := middleware.GetRequestID(ctx)

	// {
	// 	"cursor": "eyJpZCI6ImU0YjJkNTg5LTk4MzItNDdkMy1iMTg0LWVlZjdiNTM5YmY1NyIsImNyZWF0ZWRfYXQiOiIyMDI2LTAzLTIwVDEwOjA1OjAwWiIsInZlcnNpb24iOjF9",
	// 	"limit": "6"
	// }

	limit, err := parseListingLimit(r.URL.Query().Get("limit"))
	if err != nil {
		httpx.ValidationError(w, http.StatusBadRequest, err.Error(), httpx.CodeValidationFailed, "limit")
		return
	}

	rawCursor := r.URL.Query().Get("cursor")
	if rawCursor == "" {
		rawCursor = r.URL.Query().Get("after")
	}

	var cursor *dtos.ListingCursor
	if rawCursor != "" {
		c, err := dtos.DecodeCursor(rawCursor)
		if err != nil {
			lh.Logger.Error("GetListings: invalid cursor", "request_id", reqId, "cursor", rawCursor, "err", err)
			httpx.ValidationError(w, http.StatusBadRequest, "invalid pagination cursor", httpx.CodeValidationFailed, "cursor")
			return
		}
		cursor = c
	}

	resp, err := lh.ListingService.GetListings(ctx, reqId, limit, cursor)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "internal server error", httpx.CodeInternalError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		lh.Logger.Error("GetListings encode error", "request_id", reqId, "err", err)
	}
}

func (lh *ListingHandler) GetListingByID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqId := middleware.GetRequestID(ctx)

	id := r.PathValue("id")
	if id == "" {
		lh.Logger.Error("GetListingByID: missing id", "request_id", reqId)
		httpx.Error(w, http.StatusBadRequest, "missing id", httpx.CodeBadRequest)
		return
	}

	if _, err := uuid.Parse(id); err != nil {
		lh.Logger.Error("GetListingByID: invalid uuid", "id", id, "request_id", reqId, "err", err)
		httpx.Error(w, http.StatusBadRequest, "invalid id format", httpx.CodeInvalidID)
		return
	}

	listing, err := lh.ListingService.GetListingByID(ctx, reqId, id)
	if err != nil {
		if errors.Is(err, services.ErrListingNotFound) {
			httpx.Error(w, http.StatusNotFound, "listing not found", httpx.CodeNotFound)
			return
		}
		httpx.Error(w, http.StatusInternalServerError, "internal server error", httpx.CodeInternalError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(listing); err != nil {
		lh.Logger.Error("GetListingByID encode error", "request_id", reqId, "err", err)
	}
}

func (lh *ListingHandler) DeleteListing(w http.ResponseWriter, r *http.Request) {

	// request scoped context
	ctx := r.Context()
	// get request id from context
	reqId := middleware.GetRequestID(ctx)

	id := r.PathValue("id")
	if id == "" {
		lh.Logger.Error("DeleteListing: missing id", "request_id", reqId)
		httpx.Error(w, http.StatusBadRequest, "missing id", httpx.CodeBadRequest)
		return
	}

	// 3. Validate UUID format
	if _, err := uuid.Parse(id); err != nil {
		lh.Logger.Error("DeleteListing: invalid uuid", "id", id, "request_id", reqId, "err", err)
		httpx.Error(w, http.StatusBadRequest, "invalid id format", httpx.CodeInvalidID)
		return
	}

	claims, claimErr := middleware.GetAuthClaims(ctx)
	if claimErr != nil {
		lh.Logger.Error("DeleteListing: failed to get claims", "request_id", reqId, "err", claimErr)
		httpx.Error(w, http.StatusUnauthorized, "unauthorized", httpx.CodeUnauthorized)
		return
	}

	rowsAffected, err := lh.ListingService.DeleteListing(ctx, reqId, id, claims.UserID)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "internal server error", httpx.CodeInternalError)
		return
	}

	if rowsAffected == 0 {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized", httpx.CodeUnauthorized)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (lh *ListingHandler) CreateListing(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	reqId := middleware.GetRequestID(ctx)

	claims, claimErr := middleware.GetAuthClaims(ctx)
	if claimErr != nil {
		lh.Logger.Error("CreateListing: failed to get claims", "request_id", reqId, "err", claimErr)
		httpx.Error(w, http.StatusUnauthorized, "unauthorized", httpx.CodeUnauthorized)
		return
	}

	var req dtos.CreateListingRequest

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		lh.Logger.Error("CreateListing decode error", "request_id", reqId, "err", err)
		httpx.Error(w, http.StatusBadRequest, "invalid request body", httpx.CodeBadRequest)
		return
	}

	listingUploadKeys, err := req.Validate(claims.UserID)
	if err != nil {
		var vError *dtos.ValidationError
		field := ""
		if errors.As(err, &vError) && vError != nil {
			field = vError.Field
		}
		lh.Logger.Error("CreateListing validate error", "request_id", reqId, "err", err)
		httpx.ValidationError(w, http.StatusUnprocessableEntity, err.Error(), httpx.CodeValidationFailed, field)
		return
	}

	out, err := lh.ListingService.CreateListing(ctx, reqId, req, claims.UserID, listingUploadKeys)
	if err != nil {
		var vError *dtos.ValidationError
		if errors.As(err, &vError) && vError != nil {
			lh.Logger.Error("CreateListing validate error", "request_id", reqId, "err", err)
			httpx.ValidationError(w, http.StatusUnprocessableEntity, vError.Message, httpx.CodeValidationFailed, vError.Field)
			return
		}

		lh.Logger.Error("CreateListing service error", "request_id", reqId, "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal server error", httpx.CodeInternalError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(out)

}

func parseListingLimit(raw string) (int, error) {
	if raw == "" {
		return DefaultListLimit, nil
	}

	limit, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("limit must be an integer value")
	}

	if limit < MinListLimit || limit > MaxListLimit {
		return 0, fmt.Errorf("limit must be between %d and %d", MinListLimit, MaxListLimit)
	}

	return limit, nil
}
