package pipeline

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/brian-l-johnson/android-re-pipeline/services/coordinator/internal/jobs"
	"github.com/brian-l-johnson/android-re-pipeline/services/coordinator/internal/metadata"
	"github.com/brian-l-johnson/android-re-pipeline/services/coordinator/internal/mobsf"
	"github.com/brian-l-johnson/android-re-pipeline/services/coordinator/internal/queue"
	"github.com/brian-l-johnson/android-re-pipeline/services/coordinator/internal/store"
)

// Orchestrator implements jobs.JobEventHandler and queue.MessageHandler.
// It ties together the k8s job watcher, NATS consumer, store, and MobSF client
// into the full APK analysis pipeline.
type Orchestrator struct {
	store   *store.Store
	manager *jobs.Manager
	mobsf   *mobsf.Client
	dataDir string
}

// NewOrchestrator creates an Orchestrator with all its dependencies.
func NewOrchestrator(s *store.Store, m *jobs.Manager, mc *mobsf.Client, dataDir string) *Orchestrator {
	return &Orchestrator{
		store:   s,
		manager: m,
		mobsf:   mc,
		dataDir: dataDir,
	}
}

// Compile-time interface assertions.
var _ jobs.JobEventHandler = (*Orchestrator)(nil)
var _ queue.MessageHandler = (*Orchestrator)(nil)

// ---------------------------------------------------------------------------
// queue.MessageHandler implementation
// ---------------------------------------------------------------------------

// HandleIngested processes an apk.ingested NATS message.
// It performs dedup, inserts the job, creates k8s Jobs, and marks the job running.
func (o *Orchestrator) HandleIngested(ctx context.Context, msg *queue.IngestedMessage) error {
	jobID, err := uuid.Parse(msg.JobID)
	if err != nil {
		return fmt.Errorf("invalid job_id %q: %w", msg.JobID, err)
	}

	submittedAt, err := time.Parse(time.RFC3339, msg.SubmittedAt)
	if err != nil {
		submittedAt = time.Now()
	}

	// Dedup: if a completed job exists for this SHA256, create a completed
	// job record for the new ID so the caller can poll it and get results
	// immediately without re-running the full analysis pipeline.
	if msg.SHA256 != "" {
		existing, err := o.store.GetJobBySHA256(ctx, msg.SHA256)
		if err == nil && existing != nil && existing.Status == "complete" {
			log.Printf("orchestrator: job %s deduped (sha256 %s already complete as %s)",
				jobID, msg.SHA256, existing.ID)
			if err := o.store.CreateJobAsDuplicate(ctx, jobID, msg.APKPath, existing, submittedAt); err != nil {
				log.Printf("orchestrator: create dedup job record failed (job=%s): %v", jobID, err)
			}
			return nil
		}
	}

	job := store.Job{
		ID:            jobID,
		Status:        "pending",
		APKPath:       msg.APKPath,
		PackageName:   msg.PackageName,
		Version:       msg.Version,
		Source:        msg.Source,
		SHA256:        msg.SHA256,
		SubmittedAt:   submittedAt,
		JadxStatus:    "pending",
		ApktoolStatus: "pending",
		MobSFStatus:   "pending",
	}

	if err := o.store.CreateJob(ctx, job); err != nil {
		return fmt.Errorf("create job in db: %w", err)
	}

	if err := o.manager.CreateAnalysisJobs(ctx, jobID, msg.APKPath); err != nil {
		_ = o.store.SetJobError(ctx, jobID, fmt.Sprintf("create k8s jobs: %v", err))
		return fmt.Errorf("create k8s jobs: %w", err)
	}

	if err := o.store.UpdateJobStatus(ctx, jobID, "running"); err != nil {
		return fmt.Errorf("update job to running: %w", err)
	}

	log.Printf("orchestrator: job %s started (apk=%s)", jobID, msg.APKPath)
	return nil
}

// HandleIngestionFailed processes an apk.ingestion.failed NATS message.
func (o *Orchestrator) HandleIngestionFailed(ctx context.Context, msg *queue.FailedMessage) error {
	jobID, err := uuid.Parse(msg.JobID)
	if err != nil {
		return fmt.Errorf("invalid job_id %q: %w", msg.JobID, err)
	}

	if err := o.store.SetJobError(ctx, jobID, msg.Error); err != nil {
		return fmt.Errorf("set job error: %w", err)
	}

	log.Printf("orchestrator: job %s ingestion failed: %s", jobID, msg.Error)
	return nil
}

// ---------------------------------------------------------------------------
// jobs.JobEventHandler implementation
// ---------------------------------------------------------------------------

// OnJobComplete is called when a jadx or apktool k8s Job completes successfully.
func (o *Orchestrator) OnJobComplete(jobID uuid.UUID, tool string) {
	ctx := context.Background()

	// Guard against re-processing already-complete events (e.g. informer resync).
	existing, err := o.store.GetJob(ctx, jobID)
	if err != nil {
		log.Printf("orchestrator: get job %s failed: %v", jobID, err)
		return
	}
	switch tool {
	case "jadx":
		if existing.JadxStatus == "complete" {
			return
		}
	case "apktool":
		if existing.ApktoolStatus == "complete" {
			return
		}
	}

	if err := o.store.UpdateJobToolStatus(ctx, jobID, tool, "complete"); err != nil {
		log.Printf("orchestrator: update tool status failed (job=%s tool=%s): %v", jobID, tool, err)
		return
	}
	log.Printf("orchestrator: tool %s complete for job %s", tool, jobID)

	// If apktool just finished, parse and store APK metadata.
	if tool == "apktool" {
		o.runMetadataParsing(ctx, jobID)
	}

	// Check whether both tools are now done.
	job, err := o.store.GetJob(ctx, jobID)
	if err != nil {
		log.Printf("orchestrator: get job %s failed: %v", jobID, err)
		return
	}

	if job.JadxStatus == "complete" && job.ApktoolStatus == "complete" && job.MobSFStatus == "pending" {
		// Mark the job complete now so results are downloadable without waiting
		// for MobSF. MobSF continues in the background and updates its own status.
		resultsPath := fmt.Sprintf("%s/output/%s", o.dataDir, jobID.String())
		if err := o.store.SetJobCompleted(ctx, jobID, resultsPath); err != nil {
			log.Printf("orchestrator: set job completed failed (job=%s): %v", jobID, err)
		} else {
			log.Printf("orchestrator: job %s marked complete (results at %s); MobSF starting in background", jobID, resultsPath)
		}
		go o.runMobSF(jobID, job.APKPath)
	}
}

// OnJobFailed is called when a jadx or apktool k8s Job fails.
func (o *Orchestrator) OnJobFailed(jobID uuid.UUID, tool string, logs string) {
	ctx := context.Background()

	if err := o.store.UpdateJobToolStatus(ctx, jobID, tool, "failed"); err != nil {
		log.Printf("orchestrator: update tool status failed (job=%s tool=%s): %v", jobID, tool, err)
	}

	errMsg := fmt.Sprintf("%s: %s", tool, logs)
	if err := o.store.SetJobError(ctx, jobID, errMsg); err != nil {
		log.Printf("orchestrator: set job error failed (job=%s): %v", jobID, err)
	}

	log.Printf("orchestrator: tool %s failed for job %s: %s", tool, jobID, logs)
}

// ReconcilePendingMobSF relaunches MobSF for any jobs that are marked complete
// but still have mobsf_status='pending'. This recovers jobs where the coordinator
// restarted between marking the job complete and the MobSF goroutine finishing.
func (o *Orchestrator) ReconcilePendingMobSF(jobs []store.Job) {
	for _, job := range jobs {
		log.Printf("orchestrator: reconcile — relaunching MobSF for job %s", job.ID)
		go o.runMobSF(job.ID, job.APKPath)
	}
}

// RetriggerMobSF resets mobsf_status to 'pending' and relaunches the MobSF
// scan goroutine. Returns an error if MobSF is already running for this job.
func (o *Orchestrator) RetriggerMobSF(ctx context.Context, job *store.Job) error {
	if job.MobSFStatus == "running" {
		return fmt.Errorf("mobsf scan already in progress")
	}
	if err := o.store.ResetJobMobSF(ctx, job.ID); err != nil {
		return fmt.Errorf("reset mobsf status: %w", err)
	}
	log.Printf("orchestrator: retriggering MobSF for job %s", job.ID)
	go o.runMobSF(job.ID, job.APKPath)
	return nil
}

// RetriggerJob resets a failed job back to running and re-creates the k8s
// analysis jobs for jadx and apktool (MobSF will follow once they complete).
func (o *Orchestrator) RetriggerJob(ctx context.Context, job *store.Job) error {
	if job.Status == "running" {
		return fmt.Errorf("job is already running")
	}
	if err := o.store.ResetJobForRetrigger(ctx, job.ID); err != nil {
		return fmt.Errorf("reset job: %w", err)
	}
	if err := o.manager.CreateAnalysisJobs(ctx, job.ID, job.APKPath); err != nil {
		_ = o.store.SetJobError(ctx, job.ID, fmt.Sprintf("retrigger: create k8s jobs: %v", err))
		return fmt.Errorf("create k8s jobs: %w", err)
	}
	log.Printf("orchestrator: job %s retriggered", job.ID)
	return nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func (o *Orchestrator) runMetadataParsing(ctx context.Context, jobID uuid.UUID) {
	outputDir := fmt.Sprintf("%s/output/%s/apktool", o.dataDir, jobID.String())

	parsed, err := metadata.ParseFromApktoolOutput(outputDir)
	if err != nil {
		log.Printf("orchestrator: metadata parse failed for job %s: %v", jobID, err)
		return
	}

	meta := store.APKMetadata{
		ID:          uuid.New(),
		JobID:       jobID,
		PackageName: parsed.PackageName,
		Version:     parsed.Version,
		VersionCode: parsed.VersionCode,
		MinSDK:      parsed.MinSDK,
		TargetSDK:   parsed.TargetSDK,
		Permissions: parsed.Permissions,
		Activities:  parsed.Activities,
		Services:    parsed.Services,
		Receivers:   parsed.Receivers,
		IngestedAt:  time.Now(),
	}

	if err := o.store.InsertAPKMetadata(ctx, meta); err != nil {
		log.Printf("orchestrator: insert apk metadata failed for job %s: %v", jobID, err)
	}

	// Backfill package_name / version onto the jobs row so the list API and
	// UI can display them without a separate apk_metadata join.
	if parsed.PackageName != "" || parsed.Version != "" {
		if err := o.store.UpdateJobPackageInfo(ctx, jobID, parsed.PackageName, parsed.Version); err != nil {
			log.Printf("orchestrator: update job package info failed for job %s: %v", jobID, err)
		}
	}
}

func (o *Orchestrator) runMobSF(jobID uuid.UUID, apkPath string) {
	ctx := context.Background()

	if err := o.store.UpdateJobToolStatus(ctx, jobID, "mobsf", "running"); err != nil {
		log.Printf("orchestrator: set mobsf running failed (job=%s): %v", jobID, err)
	}

	scanHash, err := o.mobsf.Upload(ctx, apkPath)
	if err != nil {
		log.Printf("orchestrator: mobsf upload failed (job=%s): %v", jobID, err)
		_ = o.store.UpdateJobToolStatus(ctx, jobID, "mobsf", "failed")
		return
	}

	result, err := o.mobsf.PollScan(ctx, scanHash)
	if err != nil {
		log.Printf("orchestrator: mobsf scan failed (job=%s): %v", jobID, err)
		_ = o.store.UpdateJobToolStatus(ctx, jobID, "mobsf", "failed")
		return
	}

	if err := o.store.SetMobSFReport(ctx, jobID, result.Report); err != nil {
		log.Printf("orchestrator: store mobsf report failed (job=%s): %v", jobID, err)
	}

	if err := o.store.UpdateJobToolStatus(ctx, jobID, "mobsf", "complete"); err != nil {
		log.Printf("orchestrator: set mobsf complete failed (job=%s): %v", jobID, err)
	}

	log.Printf("orchestrator: mobsf complete for job %s", jobID)
}
