package handlers

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/tecisage/resell-apis/internal/dtos"
	"github.com/tecisage/resell-apis/internal/httpx"
	"github.com/tecisage/resell-apis/internal/middlewares"
	"github.com/tecisage/resell-apis/internal/services"
	"github.com/tecisage/resell-apis/internal/storage"
)

type UploadHandler struct {
	Logger        *slog.Logger
	R2Client      *storage.Client
	UploadService *services.UploadService
}

func NewUploadHandler(logger *slog.Logger, r2Client *storage.Client) *UploadHandler {
	uploadService := services.NewUploadService(logger, r2Client)
	return &UploadHandler{
		Logger:        logger,
		R2Client:      r2Client,
		UploadService: uploadService,
	}
}

func (uh *UploadHandler) Presign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestId := middlewares.GetRequestID(ctx)
	claims, err := middlewares.GetAuthClaims(ctx)

	if err != nil {
		uh.Logger.Error("Error getting claims", "error", err, "request_id", requestId)
		httpx.Error(w, http.StatusUnauthorized, "unauthorized", httpx.CodeUnauthorized)
		return
	}

	userUUID := claims.UserID

	// decode req
	var req dtos.PresignReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		uh.Logger.Error("Error decoding request", "error", err, "request_id", requestId)
		httpx.Error(w, http.StatusBadRequest, "invalid request body", httpx.CodeBadRequest)
		return
	}

	// validate req
	presignedFileKeys, err := req.ValidateAndGetUploadKeys(userUUID)
	if err != nil {
		var vError *dtos.ValidationError
		field := ""
		msg := err.Error()
		if errors.As(err, &vError) && vError != nil {
			field = vError.Field
			msg = vError.Message
		}
		uh.Logger.Error("Presign validate error", "request_id", requestId, "err", err)
		httpx.ValidationError(w, http.StatusUnprocessableEntity, msg, httpx.CodeValidationFailed, field)
		return
	}

	// create presigned urls
	presignedURLS, err := uh.UploadService.GetPresignedURLs(ctx, presignedFileKeys, dtos.PresignTTL)
	if err != nil {
		uh.Logger.Error("Error getting presigned urls", "error", err, "request_id", requestId)
		httpx.Error(w, http.StatusInternalServerError, "failed to get presigned urls", httpx.CodeInternalError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(presignedURLS)
}
