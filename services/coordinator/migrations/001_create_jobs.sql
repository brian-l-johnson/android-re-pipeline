-- +goose Up
CREATE TABLE jobs (
    id UUID PRIMARY KEY,
    status TEXT NOT NULL DEFAULT 'pending',
    apk_path TEXT NOT NULL,
    package_name TEXT,
    version TEXT,
    source TEXT,
    sha256 TEXT,
    submitted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    error TEXT,
    results_path TEXT,
    jadx_status TEXT NOT NULL DEFAULT 'pending',
    apktool_status TEXT NOT NULL DEFAULT 'pending',
    mobsf_status TEXT NOT NULL DEFAULT 'pending',
    mobsf_report JSONB
);

CREATE INDEX idx_jobs_status ON jobs(status);
CREATE INDEX idx_jobs_sha256 ON jobs(sha256);

-- +goose Down
DROP TABLE jobs;
