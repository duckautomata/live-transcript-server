package store

import (
	"context"
	"database/sql"

	"live-transcript-server/internal/model"
)

// UpsertWorkerStatus updates the status of a worker for a specific key.
func (s *Store) UpsertWorkerStatus(ctx context.Context, key, version, buildTime string, lastSeen int64) error {
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO worker_status (channel_key, worker_version, worker_build_time, last_seen)
	VALUES (?, ?, ?, ?)
	ON CONFLICT(channel_key) DO UPDATE SET
		worker_version = excluded.worker_version,
		worker_build_time = excluded.worker_build_time,
		last_seen = excluded.last_seen;
	`, key, version, buildTime, lastSeen)
	return err
}

// GetWorkerStatusByKey returns the worker status for a single channel, or nil
// if no status row exists yet.
func (s *Store) GetWorkerStatusByKey(ctx context.Context, channelKey string) (*model.WorkerStatus, error) {
	row := s.db.QueryRowContext(ctx, "SELECT channel_key, worker_version, worker_build_time, last_seen FROM worker_status WHERE channel_key = ?", channelKey)
	var st model.WorkerStatus
	err := row.Scan(&st.ChannelKey, &st.WorkerVersion, &st.WorkerBuildTime, &st.LastSeen)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// GetAllWorkerStatus retrieves the status of all workers.
func (s *Store) GetAllWorkerStatus(ctx context.Context) ([]model.WorkerStatus, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT channel_key, worker_version, worker_build_time, last_seen FROM worker_status ORDER BY last_seen DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var statuses []model.WorkerStatus
	for rows.Next() {
		var st model.WorkerStatus
		if err := rows.Scan(&st.ChannelKey, &st.WorkerVersion, &st.WorkerBuildTime, &st.LastSeen); err != nil {
			return nil, err
		}
		statuses = append(statuses, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return statuses, nil
}

// ResetWorkerStatus clears the worker_status table.
func (s *Store) ResetWorkerStatus(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM worker_status")
	return err
}

// UpsertRestartRequest marks the given channel as needing a worker restart by
// recording the request timestamp. Repeated calls overwrite the timestamp so
// the worker always sees the most recent request.
func (s *Store) UpsertRestartRequest(ctx context.Context, channelKey string, requestedAt int64) error {
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO worker_restart_requests (channel_key, requested_at)
	VALUES (?, ?)
	ON CONFLICT(channel_key) DO UPDATE SET requested_at = excluded.requested_at;
	`, channelKey, requestedAt)
	return err
}

// GetRestartRequest returns the requested_at timestamp for a pending restart,
// or 0 if none is pending.
func (s *Store) GetRestartRequest(ctx context.Context, channelKey string) (int64, error) {
	var requestedAt int64
	err := s.db.QueryRowContext(ctx, "SELECT requested_at FROM worker_restart_requests WHERE channel_key = ?", channelKey).Scan(&requestedAt)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return requestedAt, nil
}

// DeleteRestartRequest clears the pending restart for a channel. Returns the
// number of rows deleted (1 if cleared, 0 if no request was pending).
func (s *Store) DeleteRestartRequest(ctx context.Context, channelKey string) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM worker_restart_requests WHERE channel_key = ?", channelKey)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DefaultWorkerID is the worker_cookie_status key used while a single worker
// serves every channel. The column exists so a second worker has somewhere to
// go without a migration.
const DefaultWorkerID = "default"

// GetCookieStatus returns the stored cookie health, or nil if the worker has
// never reported any.
func (s *Store) GetCookieStatus(ctx context.Context, workerID string) (*model.CookieStatus, error) {
	row := s.db.QueryRowContext(ctx, `
	SELECT worker_id, state, reason, since, alerted, updated_at
	FROM worker_cookie_status WHERE worker_id = ?`, workerID)
	var st model.CookieStatus
	err := row.Scan(&st.WorkerID, &st.State, &st.Reason, &st.Since, &st.Alerted, &st.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// UpsertCookieStatus records the worker's latest cookie health. `since` is
// preserved across repeated reports of the same state so the server can
// measure how long the outage has run from its own clock.
func (s *Store) UpsertCookieStatus(ctx context.Context, st model.CookieStatus) error {
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO worker_cookie_status (worker_id, state, reason, since, alerted, updated_at)
	VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT(worker_id) DO UPDATE SET
		state = excluded.state,
		reason = excluded.reason,
		since = excluded.since,
		alerted = excluded.alerted,
		updated_at = excluded.updated_at;
	`, st.WorkerID, st.State, st.Reason, st.Since, st.Alerted, st.UpdatedAt)
	return err
}
