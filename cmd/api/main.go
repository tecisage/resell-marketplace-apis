package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tecisage/resell-apis/internal/config"
	"github.com/tecisage/resell-apis/internal/db"
	"github.com/tecisage/resell-apis/internal/handlers"
	middleware "github.com/tecisage/resell-apis/internal/middlewares"
	"github.com/tecisage/resell-apis/internal/storage"
	"github.com/tecisage/resell-apis/internal/worker"
	"golang.org/x/time/rate"
)

const (
	serverShutdownTimeout = 15 * time.Second
	workerShutdownTimeout = 10 * time.Second
)

func main() {

	// load config
	cfg := config.MustLoad()

	// interupt comes when you press ctrl+c
	// syscall.SIGTERM comes when you press kill command --> comes mainly from the orchestrators (kubernetes, Docker)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// DB connect
	db, err := db.Connect(cfg.DB_URL)
	if err != nil {
		log.Fatalf("main.db.Connect: %v", err)
	}
	defer db.Close()

	// logging
	loggerHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		AddSource: true,
		Level:     slog.LevelInfo,
	})
	logger := slog.New(loggerHandler)
	slog.SetDefault(logger)

	// Object Storage Initialisation
	r2Client, err := storage.NewR2Client(ctx, storage.R2Config{
		AccountID:    cfg.R2_ACCOUNT_ID,
		AccessKey:    cfg.R2_ACCESS_KEY,
		AccessSecret: cfg.R2_ACCESS_SECRET,
		Bucket:       cfg.R2_BUCKET,
	})
	if err != nil {
		log.Fatalf("main.storage.NewR2Client: %v", err)
	}

	// worker
	worker := worker.NewWorker(db, r2Client, logger)
	go worker.Run(ctx)

	// create mux
	mux := http.NewServeMux()

	// health check
	mux.HandleFunc("GET /healthz", handlers.Healthz)

	// rate limiting

	// 60 req per second --> 1 token/sec, burst 60
	globalLimiter := middleware.RateLimitMiddleware(logger, rate.Every(time.Second), 100, cfg)

	signInRateLimiter := middleware.RateLimitMiddleware(logger, rate.Every(time.Second*12), 5, cfg)
	// rate.Every(time.Second*12) --> means it will take 12 seconds to fill one token. 1/12 tokens per second --> 5 req/min
	// burst 5 --> means it will allow 5 requests at once (max capacity of bucket for an IP)
	// for login we allow only 5 requests per minute per user

	// ====== If you want to allow 500 req per min per user ======
	// 500 req/ min ==> 60 sec/req per minute ==> 60/500 = 0.12 sec/req per minute
	//means it will take 0.12 seconds to fill one token and it will allow 500 requests at once (max capacity of bucket for an IP)
	// SignInRateLimiter := middleware.RateLimitMiddleware(logger, rate.Every(time.Millisecond*120), 500, cfg)
	// =======================
	signUpRateLimiter := middleware.RateLimitMiddleware(logger, rate.Every(time.Minute*1), 3, cfg)

	// auth routes
	authHandler := handlers.NewAuthHandler(db, logger, cfg.JWTSecret)
	mux.Handle("POST /auth/signup", signUpRateLimiter(http.HandlerFunc(authHandler.Signup)))
	mux.Handle("POST /auth/login", signInRateLimiter(http.HandlerFunc(authHandler.Login)))
	mux.HandleFunc("POST /auth/logout", authHandler.Logout)

	// 2. Protect routes (e.g. creating/deleting listings)
	requireAuth := middleware.AuthMiddleware(cfg.JWTSecret)

	// listing routes
	listingHandler := handlers.NewListingHandler(db, logger, r2Client)
	mux.HandleFunc("GET /listings", listingHandler.GetListings)
	mux.HandleFunc("GET /listings/{id}", listingHandler.GetListingByID)
	mux.Handle("DELETE /listings/{id}", requireAuth(http.HandlerFunc(listingHandler.DeleteListing)))
	mux.Handle("POST /listings", requireAuth(http.HandlerFunc(listingHandler.CreateListing)))

	// Upload routes
	uploadHandler := handlers.NewUploadHandler(logger, r2Client)
	mux.Handle("POST /uploads/presign", requireAuth(http.HandlerFunc(uploadHandler.Presign)))

	// middlewares
	corsMiddleware := middleware.CORSMiddleware(cfg.AllowedOrigins)
	rootHandler := globalLimiter(middleware.RequestIDMiddleware(corsMiddleware(mux)))

	// create http server
	log.Println("Starting server on :" + cfg.Port)
	srv := http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           rootHandler,
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// start server - we have to. move this into a seprate goroutine because ListenAndServe is a blocking call and we can't proceed to other instructions below it.
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("main.srv.ListenAndServe: %v", err)
		}
	}()

	// wait for interrupt signal - unblock when we receive the interrupt signal (ctrl + c) or kill command
	<-ctx.Done()
	stop()

	// shutdown server gracefully or Clear the resources
	// give 1 minute to server to shut down gracefully
	logger.Info("Shutdown server gracefully", "serverShutdownTimeout", serverShutdownTimeout)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), serverShutdownTimeout)
	defer shutdownCancel()

	// 1) stop accepting new requests
	// 2) wait for existing requests to finish
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("main.srv.Shutdown: %v", err)
	}

	// 3) clean up resources (close db, close r2 client, etc)
	// wait for worker to finish all
	select {
	case <-worker.Done():
		logger.Info("worker shutted down")
	case <-time.After(workerShutdownTimeout):
		// here we don't have any option to force shutdown the worker
		// because done channel is not closed and worker is not accepting
		logger.Warn("worker is not shutted down yet , forcefull shutdown", "Timeout", workerShutdownTimeout)
	}

	db.Close()
	logger.Info("server shutted down")
}
