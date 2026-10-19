package worker

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/tecisage/resell-apis/internal/dtos"
	"github.com/tecisage/resell-apis/internal/imaging"
	"github.com/tecisage/resell-apis/internal/jobs"
	"github.com/tecisage/resell-apis/internal/storage"
)

const (
	MaxEdge         = 2048
	ImageQuality    = 85
	maxAttempts     = 4
	JobPollInterval = 3 * time.Second
)

var ErrPermanent = errors.New("permanent failure")

func permanentf(format string, args ...any) error {
	return fmt.Errorf("%w:%s", ErrPermanent, fmt.Sprintf(format, args...))
}

type Worker struct {
	db      *sql.DB
	storage *storage.Client
	logger  *slog.Logger
	done    chan struct{}
}

func NewWorker(db *sql.DB, storage *storage.Client, logger *slog.Logger) *Worker {
	return &Worker{
		db:      db,
		storage: storage,
		logger:  logger,
		done:    make(chan struct{}),
	}
}

func (w *Worker) Run(ctx context.Context) {
	w.logger.Info("Worker Started")
	defer close(w.done)
	for {

		select {
		case <-ctx.Done():
			w.logger.Info("Worker Stopped")
			// now we have two way to unblock the worker at main one way is close done channel and other way is send data on the done channel.
			// if we close done channel then main will not able to receive data from the done channel and will get unblock.
			// if we send data on the done channel then main will receive and get unblock but we can't close the done channel because it is already open and we have already send data on it.
			// so we have to choose one way to unblock the worker done at main.

			// w.done <- struct{}{} -- sending data on the done channel - this will unblock main()
			// close(w.done)  --> handled by defer - closing the done channel - this will unblock main()
			return
		default:
		}

		isEmpty, err := w.claimAndProcess(ctx)
		if err != nil {
			w.logger.Error("Error claiming job", "error", err)
			continue
		}
		if isEmpty {

			select {
			case <-ctx.Done():
				w.logger.Info("Worker Stopped")
				// close(w.done) --> handled by defer
				return
			case <-time.After(JobPollInterval):
				w.logger.Info("No jobs found , will retry after ", "JobPollInterval", JobPollInterval)
				// continue to next iteration
			}
		}
	}
}

func (w *Worker) Done() <-chan struct{} {
	return w.done
}

func (w *Worker) claimAndProcess(ctx context.Context) (bool, error) {

	var (
		id       int64
		kind     string
		payload  []byte
		attempts int
	)

	query := ` UPDATE jobs 
		SET attempts = attempts + 1 ,
		run_after = NOW() + LEAST(
			(INTERVAL '30 seconds') * POWER(2, attempts),
			INTERVAL '1 hour' )
		WHERE id = (
				SELECT id FROM jobs 
				WHERE run_after <= NOW()
				AND attempts < $1
				ORDER BY run_after, id
				FOR UPDATE SKIP LOCKED 
				LIMIT 1
			)
		RETURNING id, kind, payload ,attempts`

	err := w.db.QueryRowContext(ctx, query, maxAttempts).Scan(&id, &kind, &payload, &attempts)
	if err != nil {
		if err == sql.ErrNoRows {
			return true, nil
		}
		return false, err
	}

	w.logger.Info("job processing started ", "id: ", id, "kind: ", kind, "attempts: ", attempts)

	if err := w.process(ctx, id, kind, payload); err != nil {
		if errors.Is(err, ErrPermanent) || attempts >= maxAttempts {
			// Mark job as failed and never run again
			go w.deadLetterJob(ctx, id, kind, payload)
			return false, nil
		}

		// Temporary failure - will be retried based on run_after logic
		w.logger.Error("Temporary failure", "job_id", id, "error", err)
		if _, err := w.db.ExecContext(ctx, `UPDATE jobs SET last_error = $1 WHERE id = $2`, err.Error(), id); err != nil {
			w.logger.Error("failed to update job", "job_id", id, "error", err)
		}
		return false, nil
	}

	return false, nil
}

func (w *Worker) process(ctx context.Context, jobID int64, kind string, payload []byte) error {

	switch kind {
	case jobs.KindProcessingListingImages:
		var p jobs.ProcessImagepayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return permanentf("invalid payload: %v", err)
		}
		return w.processImages(ctx, jobID, &p)

	default:
		return permanentf("unsupported job kind: %q", kind)
	}
}

func (w *Worker) processImages(ctx context.Context, jobID int64, payload *jobs.ProcessImagepayload) error {

	log := w.logger.With("job_id", jobID, "listing_id", payload.ListingID)

	// Fetch and normalize images in a single loop to minimize memory usage
	normalizedImages := make([][]byte, 0, len(payload.ImageSources))
	for _, src := range payload.ImageSources {
		rc, err := w.storage.GetObject(ctx, src.OriginalKey)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				log.Warn("object not found in storage, skipping", "originalKey", src.OriginalKey)
				return w.finishImageProcessing(ctx, jobID, payload.ListingID, dtos.ListingStatusRejected)
			}
			return fmt.Errorf("failed to get object %q: %w", src.OriginalKey, err)
		}

		// Stream directly from storage reader into Normalize, avoiding buffering raw image bytes in memory
		out, err := imaging.Normalize(rc, MaxEdge, ImageQuality)
		_ = rc.Close()
		if err != nil {
			return permanentf("normalizing image %q: %v", src.OriginalKey, err)
		}

		normalizedImages = append(normalizedImages, out)
	}

	// promotion
	for i, img := range normalizedImages {
		// put image in R2
		if err := w.storage.PutObject(ctx, payload.ImageSources[i].FinalKey, bytes.NewReader(img)); err != nil {
			// Todo : add image delete job to outbox table and call a job in jobs table to delete the image
			return fmt.Errorf("failed to put object %q: %w", payload.ImageSources[i].FinalKey, err)
		}
	}

	return w.finishImageProcessing(ctx, jobID, payload.ListingID, dtos.ListingStatusReady)
}

func (w *Worker) finishImageProcessing(ctx context.Context, jobID int64, listingID uuid.UUID, status dtos.ListingStatusType) error {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	defer func() {
		_ = tx.Rollback() // if already commited then this is no-op
	}()

	// update the status of listing
	_, err = tx.ExecContext(ctx, "UPDATE listings SET status = $1, updated_at = NOW() WHERE id = $2", status, listingID)
	if err != nil {
		return fmt.Errorf("failed to update listing %q status %q: %w", listingID, status, err)
	}

	// deleter the job
	_, err = tx.ExecContext(ctx, "DELETE FROM jobs WHERE id = $1", jobID)
	if err != nil {
		return fmt.Errorf("failed to delete job %d: %w", jobID, err)
	}

	// commit the transaction
	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

func (w *Worker) deadLetterJob(ctx context.Context, jobID int64, kind string, payload []byte) {
	// todo :
	// option 1 : make a new job and put in outbox table for send notification to user
	// option 2 : delete the job, mark listing as rejected  -> finishImageProcessing() - we chose it

	// first check if listing id existis yes -> call finishImageProcessing()
	// if no -> handle it differently

	listingID, ok := w.listingIDFor(kind, payload)
	if !ok {
		w.logger.Error("job dead letter no resolved listing id")
		if _, err := w.db.ExecContext(ctx, `DELETE FROM jobs WHERE id = $1`, jobID); err != nil {
			w.logger.Error("failed to delete job", "job_id", jobID, "error", err)
		}
		return
	}

	err := w.finishImageProcessing(ctx, jobID, listingID, dtos.ListingStatusRejected)
	if err != nil {
		w.logger.Error("failed to finish image processing", "job_id", jobID, "error", err)
	}
}

func (w *Worker) listingIDFor(kind string, payload []byte) (uuid.UUID, bool) {
	switch kind {
	case jobs.KindProcessingListingImages:
		var p jobs.ProcessImagepayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return uuid.Nil, false
		}
		return p.ListingID, true
	default:
		// no listing id for this job kind
		return uuid.Nil, false
	}
}
