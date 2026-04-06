-- +goose Up
CREATE TABLE apk_metadata (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id UUID REFERENCES jobs(id) ON DELETE CASCADE,
    package_name TEXT NOT NULL,
    version TEXT,
    version_code INT,
    sha256 TEXT,
    cert_sha256 TEXT,
    min_sdk INT,
    target_sdk INT,
    permissions TEXT[],
    activities TEXT[],
    services TEXT[],
    receivers TEXT[],
    ingested_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_apk_metadata_job_id ON apk_metadata(job_id);
CREATE INDEX idx_apk_metadata_package ON apk_metadata(package_name);

-- +goose Down
DROP TABLE apk_metadata;
