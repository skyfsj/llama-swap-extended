package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	ModelDownloadQueued      = "queued"
	ModelDownloadRetrying    = "retrying"
	ModelDownloadDownloading = "downloading"
	ModelDownloadCompleted   = "completed"
	ModelDownloadFailed      = "failed"
	ModelDownloadCanceled    = "canceled"
)

var (
	ErrModelDownloadNotFound    = errors.New("model download not found")
	ErrModelDownloadNotOwned    = errors.New("model download is not owned by this worker")
	ErrModelDownloadNotTerminal = errors.New("model download is still active")
)

// ModelDownloadTask is the durable state of one repository download. The
// actual bytes are kept in the destination's .part files; SQLite only stores
// queue metadata and progress so a process restart can safely resume them.
type ModelDownloadTask struct {
	ID              string    `json:"id"`
	Provider        string    `json:"provider"`
	RepoID          string    `json:"repo_id"`
	Revision        string    `json:"revision"`
	SourceID        string    `json:"source_id"`
	Include         []string  `json:"include,omitempty"`
	Exclude         []string  `json:"exclude,omitempty"`
	Status          string    `json:"status"`
	CurrentFile     string    `json:"current_file,omitempty"`
	TotalFiles      int       `json:"total_files"`
	CompletedFiles  int       `json:"completed_files"`
	TotalBytes      int64     `json:"total_bytes"`
	DownloadedBytes int64     `json:"downloaded_bytes"`
	Attempts        int       `json:"attempts"`
	NextRetryAt     time.Time `json:"next_retry_at,omitempty"`
	Error           string    `json:"error,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	StartedAt       time.Time `json:"started_at,omitempty"`
	FinishedAt      time.Time `json:"finished_at,omitempty"`
}

const modelDownloadColumns = `id,provider,repo_id,revision,source_id,include_json,exclude_json,status,current_file,total_files,completed_files,total_bytes,downloaded_bytes,attempts,next_retry_at,error,created_at,updated_at,started_at,finished_at`

type modelDownloadScanner interface {
	Scan(dest ...any) error
}

func (s *Store) CreateModelDownload(ctx context.Context, task ModelDownloadTask) error {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil {
		return errors.New("store is not initialized")
	}
	if strings.TrimSpace(task.ID) == "" || strings.TrimSpace(task.RepoID) == "" || strings.TrimSpace(task.SourceID) == "" {
		return errors.New("model download id, repository, and source are required")
	}
	task.Provider = strings.ToLower(strings.TrimSpace(task.Provider))
	if task.Provider == "" {
		task.Provider = "huggingface"
	}
	if task.Revision == "" {
		if task.Provider == "modelscope" {
			task.Revision = "master"
		} else {
			task.Revision = "main"
		}
	}
	if task.Status == "" {
		task.Status = ModelDownloadQueued
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = time.Now().UTC()
	}
	if task.UpdatedAt.IsZero() {
		task.UpdatedAt = task.CreatedAt
	}
	include, err := json.Marshal(nonNilStrings(task.Include))
	if err != nil {
		return fmt.Errorf("marshal model download include: %w", err)
	}
	exclude, err := json.Marshal(nonNilStrings(task.Exclude))
	if err != nil {
		return fmt.Errorf("marshal model download exclude: %w", err)
	}
	var startedAt, finishedAt any
	nextRetryAt := int64(0)
	if !task.NextRetryAt.IsZero() {
		nextRetryAt = task.NextRetryAt.UTC().Unix()
	}
	if !task.StartedAt.IsZero() {
		startedAt = task.StartedAt.Unix()
	}
	if !task.FinishedAt.IsZero() {
		finishedAt = task.FinishedAt.Unix()
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO model_downloads
		(id,provider,repo_id,revision,source_id,include_json,exclude_json,status,current_file,total_files,completed_files,total_bytes,downloaded_bytes,attempts,next_retry_at,error,created_at,updated_at,started_at,finished_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		task.ID, task.Provider, task.RepoID, task.Revision, task.SourceID, string(include), string(exclude), task.Status,
		task.CurrentFile, task.TotalFiles, task.CompletedFiles, task.TotalBytes, task.DownloadedBytes, task.Attempts,
		nextRetryAt, task.Error, task.CreatedAt.Unix(), task.UpdatedAt.Unix(), startedAt, finishedAt)
	if err != nil {
		return fmt.Errorf("create model download: %w", err)
	}
	return nil
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func (s *Store) GetModelDownload(ctx context.Context, id string) (ModelDownloadTask, bool, error) {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil {
		return ModelDownloadTask{}, false, errors.New("store is not initialized")
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+modelDownloadColumns+` FROM model_downloads WHERE id=?`, strings.TrimSpace(id))
	task, err := scanModelDownload(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ModelDownloadTask{}, false, nil
	}
	if err != nil {
		return ModelDownloadTask{}, false, err
	}
	return task, true, nil
}

func (s *Store) ListModelDownloads(ctx context.Context, limit, offset int) ([]ModelDownloadTask, error) {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil {
		return nil, errors.New("store is not initialized")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+modelDownloadColumns+` FROM model_downloads ORDER BY created_at DESC,id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := make([]ModelDownloadTask, 0)
	for rows.Next() {
		task, scanErr := scanModelDownload(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func (s *Store) FindActiveModelDownload(ctx context.Context, provider, repoID, revision, sourceID string, include, exclude []string) (ModelDownloadTask, bool, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return ModelDownloadTask{}, false, err
	}
	includeJSON, err := json.Marshal(nonNilStrings(include))
	if err != nil {
		return ModelDownloadTask{}, false, err
	}
	excludeJSON, err := json.Marshal(nonNilStrings(exclude))
	if err != nil {
		return ModelDownloadTask{}, false, err
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+modelDownloadColumns+` FROM model_downloads
		WHERE provider=? AND repo_id=? AND revision=? AND source_id=? AND include_json=? AND exclude_json=?
		AND status IN (?,?,?) ORDER BY created_at ASC,id ASC LIMIT 1`,
		provider, repoID, revision, sourceID, string(includeJSON), string(excludeJSON), ModelDownloadQueued, ModelDownloadRetrying, ModelDownloadDownloading)
	task, err := scanModelDownload(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ModelDownloadTask{}, false, nil
	}
	if err != nil {
		return ModelDownloadTask{}, false, err
	}
	return task, true, nil
}

// RequeueStaleModelDownloads makes tasks from a crashed process available to
// a new worker. The lease window prevents two servers during a graceful
// configuration reload from taking the same active task.
func (s *Store) RequeueStaleModelDownloads(ctx context.Context, before time.Time) error {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE model_downloads SET status=?,worker_id='',next_retry_at=0,updated_at=?,error=''
		WHERE status=? AND updated_at<?`, ModelDownloadQueued, time.Now().UTC().Unix(), ModelDownloadDownloading, before.Unix())
	return err
}

func (s *Store) ClaimNextModelDownload(ctx context.Context, workerID string, now time.Time) (ModelDownloadTask, bool, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return ModelDownloadTask{}, false, err
	}
	if strings.TrimSpace(workerID) == "" {
		return ModelDownloadTask{}, false, errors.New("model download worker id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ModelDownloadTask{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM model_downloads
		WHERE (status=? OR (status=? AND next_retry_at<=?))
		ORDER BY created_at ASC,id ASC LIMIT 1`, ModelDownloadQueued, ModelDownloadRetrying, now.Unix()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ModelDownloadTask{}, false, nil
	}
	if err != nil {
		return ModelDownloadTask{}, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE model_downloads SET status=?,worker_id=?,attempts=attempts+1,next_retry_at=0,updated_at=?,started_at=COALESCE(started_at,?),finished_at=NULL,error=''
		WHERE id=? AND (status=? OR (status=? AND next_retry_at<=?))`, ModelDownloadDownloading, workerID, now.Unix(), now.Unix(), id, ModelDownloadQueued, ModelDownloadRetrying, now.Unix())
	if err != nil {
		return ModelDownloadTask{}, false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return ModelDownloadTask{}, false, err
	}
	if changed != 1 {
		return ModelDownloadTask{}, false, nil
	}
	task, err := scanModelDownload(tx.QueryRowContext(ctx, `SELECT `+modelDownloadColumns+` FROM model_downloads WHERE id=?`, id))
	if err != nil {
		return ModelDownloadTask{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return ModelDownloadTask{}, false, err
	}
	return task, true, nil
}

func (s *Store) UpdateModelDownloadManifest(ctx context.Context, id, workerID string, totalFiles int, totalBytes int64) error {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE model_downloads SET total_files=?,total_bytes=?,updated_at=?
		WHERE id=? AND status=? AND worker_id=?`, totalFiles, totalBytes, time.Now().UTC().Unix(), id, ModelDownloadDownloading, workerID)
	if err != nil {
		return err
	}
	return modelDownloadOwnershipResult(result)
}

func (s *Store) UpdateModelDownloadProgress(ctx context.Context, id, workerID, currentFile string, completedFiles, totalFiles int, downloadedBytes, totalBytes int64) error {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE model_downloads SET current_file=?,completed_files=?,total_files=?,downloaded_bytes=?,total_bytes=?,updated_at=?
		WHERE id=? AND status=? AND worker_id=?`, currentFile, completedFiles, totalFiles, downloadedBytes, totalBytes, time.Now().UTC().Unix(), id, ModelDownloadDownloading, workerID)
	if err != nil {
		return err
	}
	return modelDownloadOwnershipResult(result)
}

func (s *Store) FinishModelDownload(ctx context.Context, id, workerID, status, message string, completedFiles int, downloadedBytes int64) error {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return err
	}
	if status != ModelDownloadCompleted && status != ModelDownloadFailed && status != ModelDownloadCanceled {
		return fmt.Errorf("invalid model download terminal status %q", status)
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE model_downloads SET status=?,error=?,completed_files=?,downloaded_bytes=?,worker_id='',next_retry_at=0,updated_at=?,finished_at=?
		WHERE id=? AND status=? AND worker_id=?`, status, message, completedFiles, downloadedBytes, now.Unix(), now.Unix(), id, ModelDownloadDownloading, workerID)
	if err != nil {
		return err
	}
	return modelDownloadOwnershipResult(result)
}

// RetryOwnedModelDownload makes a transiently failed task visible as retrying
// until its next attempt. Its partial files remain on disk and are reused by
// either normal Range resume or the chunk manifest.
func (s *Store) RetryOwnedModelDownload(ctx context.Context, id, workerID, message string, retryAt time.Time) error {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return err
	}
	if retryAt.IsZero() {
		return errors.New("model download retry time is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE model_downloads
		SET status=?,current_file='',error=?,worker_id='',next_retry_at=?,updated_at=?,finished_at=NULL
		WHERE id=? AND status=? AND worker_id=?`,
		ModelDownloadRetrying, message, retryAt.UTC().Unix(), time.Now().UTC().Unix(), id, ModelDownloadDownloading, workerID)
	if err != nil {
		return err
	}
	return modelDownloadOwnershipResult(result)
}

func (s *Store) RequeueOwnedModelDownload(ctx context.Context, id, workerID string) error {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE model_downloads SET status=?,worker_id='',next_retry_at=0,updated_at=?,error=''
		WHERE id=? AND status=? AND worker_id=?`, ModelDownloadQueued, time.Now().UTC().Unix(), id, ModelDownloadDownloading, workerID)
	if err != nil {
		return err
	}
	return modelDownloadOwnershipResult(result)
}

func modelDownloadOwnershipResult(result sql.Result) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrModelDownloadNotOwned
	}
	return nil
}

func (s *Store) CancelModelDownload(ctx context.Context, id string) error {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return err
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE model_downloads SET status=?,worker_id='',next_retry_at=0,updated_at=?,finished_at=?
		WHERE id=? AND status IN (?,?,?)`, ModelDownloadCanceled, now.Unix(), now.Unix(), id, ModelDownloadQueued, ModelDownloadRetrying, ModelDownloadDownloading)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		_, found, findErr := s.GetModelDownload(ctx, id)
		if findErr != nil {
			return findErr
		}
		if !found {
			return ErrModelDownloadNotFound
		}
	}
	return nil
}

func (s *Store) RetryModelDownload(ctx context.Context, id string) error {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE model_downloads SET status=?,worker_id='',error='',finished_at=NULL,attempts=0,next_retry_at=0,updated_at=?
		WHERE id=? AND status IN (?,?)`, ModelDownloadQueued, time.Now().UTC().Unix(), id, ModelDownloadFailed, ModelDownloadCanceled)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		_, found, findErr := s.GetModelDownload(ctx, id)
		if findErr != nil {
			return findErr
		}
		if !found {
			return ErrModelDownloadNotFound
		}
		return errors.New("model download is not failed or canceled")
	}
	return nil
}

// DeleteModelDownload removes only terminal queue history. Downloaded model
// files and .part files are intentionally left in place so deleting a task
// cannot remove data that may be shared by another task or cache entry.
func (s *Store) DeleteModelDownload(ctx context.Context, id string) error {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	result, err := s.db.ExecContext(ctx, `DELETE FROM model_downloads
		WHERE id=? AND status IN (?,?,?)`, id, ModelDownloadCompleted, ModelDownloadFailed, ModelDownloadCanceled)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed > 0 {
		return nil
	}
	_, found, findErr := s.GetModelDownload(ctx, id)
	if findErr != nil {
		return findErr
	}
	if !found {
		return ErrModelDownloadNotFound
	}
	return ErrModelDownloadNotTerminal
}

func scanModelDownload(scanner modelDownloadScanner) (ModelDownloadTask, error) {
	var task ModelDownloadTask
	var includeJSON, excludeJSON string
	var createdAt, updatedAt, nextRetryAt int64
	var startedAt, finishedAt sql.NullInt64
	if err := scanner.Scan(&task.ID, &task.Provider, &task.RepoID, &task.Revision, &task.SourceID, &includeJSON, &excludeJSON, &task.Status, &task.CurrentFile,
		&task.TotalFiles, &task.CompletedFiles, &task.TotalBytes, &task.DownloadedBytes, &task.Attempts, &nextRetryAt, &task.Error,
		&createdAt, &updatedAt, &startedAt, &finishedAt); err != nil {
		return ModelDownloadTask{}, err
	}
	if err := json.Unmarshal([]byte(includeJSON), &task.Include); err != nil {
		return ModelDownloadTask{}, fmt.Errorf("decode model download include: %w", err)
	}
	if err := json.Unmarshal([]byte(excludeJSON), &task.Exclude); err != nil {
		return ModelDownloadTask{}, fmt.Errorf("decode model download exclude: %w", err)
	}
	task.Include = nonNilStrings(task.Include)
	task.Exclude = nonNilStrings(task.Exclude)
	task.CreatedAt = time.Unix(createdAt, 0).UTC()
	task.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	if nextRetryAt > 0 {
		task.NextRetryAt = time.Unix(nextRetryAt, 0).UTC()
	}
	if startedAt.Valid {
		task.StartedAt = time.Unix(startedAt.Int64, 0).UTC()
	}
	if finishedAt.Valid {
		task.FinishedAt = time.Unix(finishedAt.Int64, 0).UTC()
	}
	return task, nil
}
