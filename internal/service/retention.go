package service

import (
	"context"
	"errors"
	"time"

	"voice-timeline-renderer/internal/domain"
)

// ReclaimSummary tallies what one retention sweep did.
type ReclaimSummary struct {
	// Expired counts outputs reclaimed because their retention period ended.
	Expired int
	// Missing counts completed jobs whose output file was already gone (for
	// example deleted by hand) and whose record was corrected to reclaimed.
	Missing int
	// Finalized counts pending deletions finished for jobs already marked
	// reclaimed (the sweep is interrupted- and restart-safe).
	Finalized int
	// Backfilled counts legacy completed jobs that had no retention record
	// and were stamped with an expiry derived from their completion time.
	Backfilled int
}

// Total counts every job the sweep had to change.
func (summary ReclaimSummary) Total() int {
	return summary.Expired + summary.Missing + summary.Finalized + summary.Backfilled
}

// ReclaimExpiredRenders applies the retention policy to every completed
// render as of now. For each expired artifact it first persists the
// reclamation fact on the job record and only then deletes the output file,
// so the job, receipt, and download endpoints never claim an artifact is
// downloadable after it is gone — not even across a process exit between the
// two steps. Downloads already in flight hold their bytes in memory and are
// never truncated; downloads that arrive after the reclamation record observe
// the reclaimed state. The sweep is idempotent and safe to run at startup and
// on a timer.
func (s *Service) ReclaimExpiredRenders(ctx context.Context, now time.Time) (ReclaimSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()

	jobs, listErr := s.repo.ListJobs()
	summary := ReclaimSummary{}
	var failures []error
	if listErr != nil {
		failures = append(failures, listErr)
	}
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if job.State != domain.RenderCompleted {
			continue
		}
		exists, err := s.repo.RenderOutputExists(job.ID)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		switch {
		case job.Reclaimed():
			// The reclamation was recorded but the file is still present
			// (the process exited between the two steps). Finish it.
			if exists {
				if err := s.repo.DeleteRenderOutput(job.ID); err != nil {
					failures = append(failures, err)
					continue
				}
				summary.Finalized++
			}
		case !exists:
			// The file vanished outside the retention flow (for example an
			// operator deleted the directory to free space). Record the
			// reclamation fact so the job stops claiming the output is
			// downloadable.
			job.MarkReclaimed(now, domain.ReclaimReasonOutputMissing)
			if err := s.repo.SaveJob(job); err != nil {
				failures = append(failures, err)
				continue
			}
			summary.Missing++
		default:
			retention := job.Retention
			if retention == nil || retention.ExpiresAt == nil {
				// Jobs completed before retention existed are stamped from
				// their completion time (UpdatedAt is the terminal
				// transition time and never moves afterwards).
				expiresAt := job.UpdatedAt.UTC().Add(s.config.RenderRetention)
				if retention == nil {
					retention = &domain.RenderRetention{}
				}
				retention.ExpiresAt = &expiresAt
				job.Retention = retention
				if now.Before(expiresAt) {
					if err := s.repo.SaveJob(job); err != nil {
						failures = append(failures, err)
					} else {
						summary.Backfilled++
					}
					continue
				}
				summary.Backfilled++
			}
			if now.Before(*retention.ExpiresAt) {
				continue
			}
			// Record the reclamation first, then delete the bytes. If the
			// process exits in between, the next sweep finalizes the
			// deletion and every endpoint already reports the artifact as
			// reclaimed.
			job.MarkReclaimed(now, domain.ReclaimReasonRetentionExpired)
			if err := s.repo.SaveJob(job); err != nil {
				failures = append(failures, err)
				continue
			}
			if err := s.repo.DeleteRenderOutput(job.ID); err != nil {
				failures = append(failures, err)
				continue
			}
			summary.Expired++
		}
	}
	return summary, errors.Join(failures...)
}
