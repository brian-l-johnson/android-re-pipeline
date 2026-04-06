package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Job represents a row in the jobs table.
type Job struct {
	ID            uuid.UUID
	Status        string // pending, running, complete, failed
	APKPath       string
	PackageName   string
	Version       string
	Source        string
	SHA256        string
	SubmittedAt   time.Time
	StartedAt     *time.Time
	CompletedAt   *time.Time
	Error         *string
	ResultsPath   *string
	JadxStatus    string
	ApktoolStatus string
	MobSFStatus   string
	MobSFReport   json.RawMessage
}

// APKMetadata represents a row in the apk_metadata table.
type APKMetadata struct {
	ID          uuid.UUID
	JobID       uuid.UUID
	PackageName string
	Version     string
	VersionCode int
	SHA256      string
	CertSHA256  string
	MinSDK      int
	TargetSDK   int
	Permissions []string
	Activities  []string
	Services    []string
	Receivers   []string
	IngestedAt  time.Time
}

// CreateJob inserts a new job into the database.
func (s *Store) CreateJob(ctx context.Context, job Job) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO jobs (
			id, status, apk_path, package_name, version, source, sha256,
			submitted_at, jadx_status, apktool_status, mobsf_status
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7,
			$8, $9, $10, $11
		)`,
		job.ID, job.Status, job.APKPath, job.PackageName, job.Version, job.Source, job.SHA256,
		job.SubmittedAt, job.JadxStatus, job.ApktoolStatus, job.MobSFStatus,
	)
	if err != nil {
		return fmt.Errorf("create job: %w", err)
	}
	return nil
}

// UpdateJobStatus updates the overall status of a job.
func (s *Store) UpdateJobStatus(ctx context.Context, jobID uuid.UUID, status string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE jobs SET status = $2
		WHERE id = $1`,
		jobID, status,
	)
	if err != nil {
		return fmt.Errorf("update job status: %w", err)
	}
	return nil
}

// UpdateJobPackageInfo backfills package_name and version on the jobs row from
// parsed apktool metadata. Only updates fields that are currently empty.
func (s *Store) UpdateJobPackageInfo(ctx context.Context, jobID uuid.UUID, packageName, version string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE jobs
		SET
			package_name = CASE WHEN package_name = '' OR package_name IS NULL THEN $2 ELSE package_name END,
			version      = CASE WHEN version      = '' OR version      IS NULL THEN $3 ELSE version      END
		WHERE id = $1`,
		jobID, packageName, version,
	)
	if err != nil {
		return fmt.Errorf("update job package info: %w", err)
	}
	return nil
}

// UpdateJobToolStatus updates jadx_status, apktool_status, or mobsf_status for a job.
func (s *Store) UpdateJobToolStatus(ctx context.Context, jobID uuid.UUID, tool string, status string) error {
	var col string
	switch tool {
	case "jadx":
		col = "jadx_status"
	case "apktool":
		col = "apktool_status"
	case "mobsf":
		col = "mobsf_status"
	default:
		return fmt.Errorf("unknown tool: %s", tool)
	}

	query := fmt.Sprintf(`UPDATE jobs SET %s = $2 WHERE id = $1`, col)
	_, err := s.pool.Exec(ctx, query, jobID, status)
	if err != nil {
		return fmt.Errorf("update job tool status (%s): %w", tool, err)
	}
	return nil
}

// SetJobError marks a job as failed with the given error message.
func (s *Store) SetJobError(ctx context.Context, jobID uuid.UUID, errMsg string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE jobs SET status = 'failed', error = $2
		WHERE id = $1`,
		jobID, errMsg,
	)
	if err != nil {
		return fmt.Errorf("set job error: %w", err)
	}
	return nil
}

// SetJobCompleted marks a job as complete and sets the results path.
func (s *Store) SetJobCompleted(ctx context.Context, jobID uuid.UUID, resultsPath string) error {
	now := time.Now()
	_, err := s.pool.Exec(ctx, `
		UPDATE jobs SET status = 'complete', results_path = $2, completed_at = $3
		WHERE id = $1`,
		jobID, resultsPath, now,
	)
	if err != nil {
		return fmt.Errorf("set job completed: %w", err)
	}
	return nil
}

// SetMobSFReport stores the MobSF JSON report for a job.
func (s *Store) SetMobSFReport(ctx context.Context, jobID uuid.UUID, report json.RawMessage) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE jobs SET mobsf_report = $2
		WHERE id = $1`,
		jobID, []byte(report),
	)
	if err != nil {
		return fmt.Errorf("set mobsf report: %w", err)
	}
	return nil
}

// GetJob retrieves a job by its ID.
func (s *Store) GetJob(ctx context.Context, jobID uuid.UUID) (*Job, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, status, apk_path, package_name, version, source, sha256,
		       submitted_at, started_at, completed_at, error, results_path,
		       jadx_status, apktool_status, mobsf_status, mobsf_report
		FROM jobs
		WHERE id = $1`,
		jobID,
	)

	job, err := scanJob(row)
	if err != nil {
		return nil, fmt.Errorf("get job: %w", err)
	}
	return job, nil
}

// GetJobBySHA256 retrieves a job by its APK SHA256 hash (for dedup checks).
func (s *Store) GetJobBySHA256(ctx context.Context, sha256 string) (*Job, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, status, apk_path, package_name, version, source, sha256,
		       submitted_at, started_at, completed_at, error, results_path,
		       jadx_status, apktool_status, mobsf_status, mobsf_report
		FROM jobs
		WHERE sha256 = $1
		ORDER BY submitted_at DESC
		LIMIT 1`,
		sha256,
	)

	job, err := scanJob(row)
	if err != nil {
		return nil, fmt.Errorf("get job by sha256: %w", err)
	}
	return job, nil
}

// ListRunningJobs returns all jobs currently in the "running" state.
func (s *Store) ListRunningJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, status, apk_path, package_name, version, source, sha256,
		       submitted_at, started_at, completed_at, error, results_path,
		       jadx_status, apktool_status, mobsf_status, mobsf_report
		FROM jobs
		WHERE status = 'running'`,
	)
	if err != nil {
		return nil, fmt.Errorf("list running jobs: %w", err)
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scan running job: %w", err)
		}
		jobs = append(jobs, *job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate running jobs: %w", err)
	}
	return jobs, nil
}

// ListJobs returns jobs ordered by submitted_at DESC with pagination.
func (s *Store) ListJobs(ctx context.Context, limit, offset int) ([]Job, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, status, apk_path, package_name, version, source, sha256,
		       submitted_at, started_at, completed_at, error, results_path,
		       jadx_status, apktool_status, mobsf_status, mobsf_report
		FROM jobs
		ORDER BY submitted_at DESC
		LIMIT $1 OFFSET $2`,
		limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		jobs = append(jobs, *job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate jobs: %w", err)
	}
	return jobs, nil
}

// CountJobs returns the total number of jobs in the database.
func (s *Store) CountJobs(ctx context.Context) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM jobs`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count jobs: %w", err)
	}
	return count, nil
}

// CreateJobAsDuplicate inserts a new job record that is immediately complete,
// copying results from an existing completed job. Used when the same APK
// SHA256 has already been fully analysed.
func (s *Store) CreateJobAsDuplicate(ctx context.Context, newJobID uuid.UUID, newAPKPath string, existing *Job, submittedAt time.Time) error {
	now := time.Now()
	resultsPath := ""
	if existing.ResultsPath != nil {
		resultsPath = *existing.ResultsPath
	}
	var report []byte
	if existing.MobSFReport != nil {
		report = []byte(existing.MobSFReport)
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO jobs (
			id, status, apk_path, package_name, version, source, sha256,
			submitted_at, completed_at, results_path,
			jadx_status, apktool_status, mobsf_status, mobsf_report
		) VALUES (
			$1, 'complete', $2, $3, $4, $5, $6,
			$7, $8, $9,
			$10, $11, $12, $13
		)`,
		newJobID, newAPKPath, existing.PackageName, existing.Version, existing.Source, existing.SHA256,
		submittedAt, now, resultsPath,
		existing.JadxStatus, existing.ApktoolStatus, existing.MobSFStatus, report,
	)
	if err != nil {
		return fmt.Errorf("create dedup job: %w", err)
	}
	return nil
}

// InsertAPKMetadata inserts APK metadata parsed from apktool output.
func (s *Store) InsertAPKMetadata(ctx context.Context, meta APKMetadata) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO apk_metadata (
			id, job_id, package_name, version, version_code, sha256, cert_sha256,
			min_sdk, target_sdk, permissions, activities, services, receivers
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7,
			$8, $9, $10, $11, $12, $13
		)`,
		meta.ID, meta.JobID, meta.PackageName, meta.Version, meta.VersionCode, meta.SHA256, meta.CertSHA256,
		meta.MinSDK, meta.TargetSDK, meta.Permissions, meta.Activities, meta.Services, meta.Receivers,
	)
	if err != nil {
		return fmt.Errorf("insert apk metadata: %w", err)
	}
	return nil
}

// GetAPKMetadata retrieves APK metadata for a given job ID.
func (s *Store) GetAPKMetadata(ctx context.Context, jobID uuid.UUID) (*APKMetadata, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, job_id, package_name, version, version_code, sha256, cert_sha256,
		       min_sdk, target_sdk, permissions, activities, services, receivers, ingested_at
		FROM apk_metadata
		WHERE job_id = $1`,
		jobID,
	)

	var meta APKMetadata
	var mobsfReport []byte // unused here, just for compat
	_ = mobsfReport

	err := row.Scan(
		&meta.ID, &meta.JobID, &meta.PackageName, &meta.Version, &meta.VersionCode,
		&meta.SHA256, &meta.CertSHA256, &meta.MinSDK, &meta.TargetSDK,
		&meta.Permissions, &meta.Activities, &meta.Services, &meta.Receivers, &meta.IngestedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("get apk metadata: %w", err)
	}
	return &meta, nil
}

// scanJob scans a single job row from a pgx.Row or pgx.Rows.
func scanJob(row pgx.Row) (*Job, error) {
	var job Job
	var mobsfReport []byte

	err := row.Scan(
		&job.ID, &job.Status, &job.APKPath, &job.PackageName, &job.Version, &job.Source, &job.SHA256,
		&job.SubmittedAt, &job.StartedAt, &job.CompletedAt, &job.Error, &job.ResultsPath,
		&job.JadxStatus, &job.ApktoolStatus, &job.MobSFStatus, &mobsfReport,
	)
	if err != nil {
		return nil, err
	}
	if mobsfReport != nil {
		job.MobSFReport = json.RawMessage(mobsfReport)
	}
	return &job, nil
}
