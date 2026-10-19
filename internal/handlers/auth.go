package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tecisage/resell-apis/internal/dtos"
	"github.com/tecisage/resell-apis/internal/httpx"
	middleware "github.com/tecisage/resell-apis/internal/middlewares"
	"github.com/tecisage/resell-apis/internal/tokens"
	"golang.org/x/crypto/bcrypt"
)

type AuthHandler struct {
	DB        *sql.DB
	Logger    *slog.Logger
	JWTSecret string
}

func NewAuthHandler(db *sql.DB, logger *slog.Logger, jwtSecret string) *AuthHandler {
	return &AuthHandler{
		DB:        db,
		Logger:    logger,
		JWTSecret: jwtSecret,
	}
}

type User struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Password  string `json:"password"`
	CreatedAt string `json:"created_at"`
}

func (ah *AuthHandler) Signup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqId := middleware.GetRequestID(ctx)

	// 1. Limit body size to prevent DoS
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var req dtos.SignUpRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		ah.Logger.Error("Signup decode error", "request_id", reqId, "err", err)
		httpx.Error(w, http.StatusBadRequest, "invalid request body", httpx.CodeBadRequest)
		return
	}

	// 2. Validate and normalize input
	if err := req.Validate(); err != nil {
		var vError *dtos.ValidationError
		field := ""
		msg := err.Error()
		if errors.As(err, &vError) && vError != nil {
			field = vError.Field
			msg = vError.Message
		}
		ah.Logger.Error("Signup validate error", "request_id", reqId, "err", err)
		httpx.ValidationError(w, http.StatusUnprocessableEntity, msg, httpx.CodeValidationFailed, field)
		return
	}

	// 3. Password Hashing
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		ah.Logger.Error("Signup password hashing error", "request_id", reqId, "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal server error", httpx.CodeInternalError)
		return
	}

	// 4. Create User Query
	createUserQuery := `INSERT INTO users (name, email, password) VALUES ($1, $2, $3) returning id, created_at`

	var u User
	err = ah.DB.QueryRowContext(ctx, createUserQuery, req.Name, req.Email, string(hashedPassword)).Scan(&u.ID, &u.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // PostgreSQL unique_violation
			httpx.Error(w, http.StatusConflict, "Email Already Taken", httpx.CodeConflict)
			return
		}
		ah.Logger.Error("Signup create user error", "request_id", reqId, "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal server error", httpx.CodeInternalError)
		return
	}

	ah.Logger.Info("User registered successfully", "user_id", u.ID, "request_id", reqId)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(dtos.SignUpResponse{ID: u.ID, Name: req.Name, Email: req.Email, CreatedAt: u.CreatedAt}); err != nil {
		ah.Logger.Error("Signup encode error", "request_id", reqId, "err", err)
	}
}

func (ah *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	reqId := middleware.GetRequestID(ctx)

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var req dtos.LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		ah.Logger.Error("Login decode error", "request_id", reqId, "err", err)
		httpx.Error(w, http.StatusBadRequest, "invalid request body", httpx.CodeBadRequest)
		return
	}

	if err := req.Validate(); err != nil {
		var vError *dtos.ValidationError
		field := ""
		msg := err.Error()
		if errors.As(err, &vError) && vError != nil {
			field = vError.Field
			msg = vError.Message
		}
		ah.Logger.Error("Signup validate error", "request_id", reqId, "err", err)
		httpx.ValidationError(w, http.StatusUnprocessableEntity, msg, httpx.CodeValidationFailed, field)
		return
	}

	// Get user by email
	getUserByEmailQuery := `SELECT id, name, password FROM users WHERE email = $1`

	var id, name, password string
	err := ah.DB.QueryRowContext(ctx, getUserByEmailQuery, req.Email).Scan(&id, &name, &password)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			ah.Logger.Warn("Login failed: user not found", "request_id", reqId)
			httpx.Error(w, http.StatusUnauthorized, "Invalid credentials", httpx.CodeUnauthorized)
			return
		}
		ah.Logger.Error("Login user query error", "request_id", reqId, "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal server error", httpx.CodeInternalError)
		return
	}

	// Compare password
	if err := bcrypt.CompareHashAndPassword([]byte(password), []byte(req.Password)); err != nil {
		ah.Logger.Warn("Login failed: password mismatch", "request_id", reqId)
		httpx.Error(w, http.StatusUnauthorized, "Invalid credentials", httpx.CodeUnauthorized)
		return
	}

	// Generate JWT
	tokenString, err := tokens.Generate(id, req.Email, ah.JWTSecret, 24*time.Hour)
	if err != nil {
		ah.Logger.Error("Login token generation error", "request_id", reqId, "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal server error", httpx.CodeInternalError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(dtos.LoginResponse{Token: tokenString})
}

func (ah *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "success"})
}
