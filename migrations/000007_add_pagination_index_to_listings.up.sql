CREATE INDEX IF NOT EXISTS idx_listings_ready_pagination
ON listings (created_at DESC, id DESC)
WHERE status = 'ready';
