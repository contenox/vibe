package runtimetypes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// AppendJobs inserts a list of jobs into the job_queue table.
func (s *store) AppendJob(ctx context.Context, job Job) error {
	if job.ID == "" {
		job.ID = uuid.New().String()
	}
	job.CreatedAt = time.Now().UTC()
	_, err := s.Exec.ExecContext(ctx, `
		INSERT INTO job_queue_v2
		(id, task_type, payload, scheduled_for, valid_until, retry_count, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7);`,
		job.ID,
		job.TaskType,
		job.Payload,
		job.ScheduledFor,
		job.ValidUntil,
		job.RetryCount,
		job.CreatedAt,
	)

	return err
}

func (s *store) AppendJobs(ctx context.Context, jobs ...*Job) error {
	if len(jobs) == 0 {
		return nil
	}
	if len(jobs) > MAXLIMIT {
		return ErrAppendLimitExceeded
	}
	now := time.Now().UTC()
	valueStrings := make([]string, 0, len(jobs))
	valueArgs := make([]interface{}, 0, len(jobs)*7)

	for i, job := range jobs {
		job.CreatedAt = now

		// Build placeholders like ($1, $2, ..., $7)
		startIdx := i*7 + 1
		placeholders := make([]string, 7)
		for j := 0; j < 7; j++ {
			placeholders[j] = fmt.Sprintf("$%d", startIdx+j)
		}
		valueStrings = append(valueStrings, "("+strings.Join(placeholders, ", ")+")")

		// Append values in the same order as columns
		valueArgs = append(valueArgs,
			job.ID,
			job.TaskType,
			job.Payload,
			job.ScheduledFor,
			job.ValidUntil,
			job.RetryCount,
			job.CreatedAt,
		)
	}

	stmt := fmt.Sprintf(`
        INSERT INTO job_queue_v2
        (id, task_type, payload, scheduled_for, valid_until, retry_count, created_at)
        VALUES %s`,
		strings.Join(valueStrings, ","),
	)

	_, err := s.Exec.ExecContext(ctx, stmt, valueArgs...)
	return err
}

// PopAllJobs removes and returns every job in the job_queue.
func (s *store) PopAllJobs(ctx context.Context) ([]*Job, error) {
	query := `
	DELETE FROM job_queue_v2
	RETURNING id, task_type, payload, scheduled_for, valid_until, retry_count, created_at;
	`
	rows, err := s.Exec.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.ID, &job.TaskType, &job.Payload, &job.ScheduledFor, &job.ValidUntil, &job.RetryCount, &job.CreatedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, &job)
	}
	return jobs, nil
}

// PopJobsForType removes and returns all jobs matching a specific task type.
func (s *store) PopJobsForType(ctx context.Context, taskType string) ([]*Job, error) {
	query := `
	DELETE FROM job_queue_v2
	WHERE task_type = $1
	RETURNING id, task_type, payload, scheduled_for, valid_until, retry_count, created_at;
	`
	rows, err := s.Exec.QueryContext(ctx, query, taskType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.ID, &job.TaskType, &job.Payload, &job.ScheduledFor, &job.ValidUntil, &job.RetryCount, &job.CreatedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, &job)
	}
	return jobs, nil
}

func (s *store) PopJobForType(ctx context.Context, taskType string) (*Job, error) {
	query := `
	DELETE FROM job_queue_v2
	WHERE id = (
		SELECT id FROM job_queue_v2 WHERE task_type = $1 ORDER BY created_at LIMIT 1
	)
	RETURNING id, task_type, payload, scheduled_for, valid_until, retry_count, created_at;
	`
	row := s.Exec.QueryRowContext(ctx, query, taskType)

	var job Job
	if err := row.Scan(&job.ID, &job.TaskType, &job.Payload, &job.ScheduledFor, &job.ValidUntil, &job.RetryCount, &job.CreatedAt); err != nil {
		return nil, err
	}

	return &job, nil
}

func (s *store) PopNJobsForType(ctx context.Context, taskType string, n int) ([]*Job, error) {
	query := `
        DELETE FROM job_queue_v2
        WHERE id IN (
            SELECT id FROM job_queue_v2
            WHERE task_type = $1
            ORDER BY created_at, id
            LIMIT $2
        )
        RETURNING id, task_type, payload, scheduled_for, valid_until, retry_count, created_at;
    `
	rows, err := s.Exec.QueryContext(ctx, query, taskType, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.ID, &job.TaskType, &job.Payload, &job.ScheduledFor, &job.ValidUntil, &job.RetryCount, &job.CreatedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, &job)
	}
	return jobs, nil
}

func (s *store) GetJobsForType(ctx context.Context, taskType string) ([]*Job, error) {
	query := `
		SELECT id, task_type, payload, scheduled_for, valid_until, retry_count, created_at
		FROM job_queue_v2
		WHERE task_type = $1
		ORDER BY created_at;
	`
	rows, err := s.Exec.QueryContext(ctx, query, taskType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.ID, &job.TaskType, &job.Payload, &job.ScheduledFor, &job.ValidUntil, &job.RetryCount, &job.CreatedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, &job)
	}
	return jobs, nil
}

func (s *store) ListJobs(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*Job, error) {
	query := `
		SELECT id, task_type, payload, scheduled_for, valid_until, retry_count, created_at
		FROM job_queue_v2
		WHERE created_at < $1
		ORDER BY created_at DESC
		LIMIT $2;
	`
	cursor := time.Now().UTC()
	if createdAtCursor != nil {
		cursor = *createdAtCursor
	}
	rows, err := s.Exec.QueryContext(ctx, query, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.ID, &job.TaskType, &job.Payload, &job.ScheduledFor, &job.ValidUntil, &job.RetryCount, &job.CreatedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, &job)
	}
	return jobs, nil
}

func (s *store) EstimateJobCount(ctx context.Context) (int64, error) {
	return s.estimateCount(ctx, "job_queue_v2")
}
