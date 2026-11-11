package checkflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"voice-timeline-renderer/internal/domain"
)

// receiptView mirrors the receipt endpoint response: the immutable receipt
// plus the job's retention record.
type receiptView struct {
	domain.RenderReceipt
	Retention *domain.RenderRetention `json:"retention"`
}

// runRetention verifies the render artifact retention lifecycle: expiry is
// stamped at completion, downloads never extend it, an expired artifact is
// reclaimed without truncating in-flight downloads, and the job, receipt, and
// output endpoints keep telling the same story across reclamation, concurrent
// reads, process restarts, and external deletions.
func runRetention(ctx context.Context, harness *Harness) error {
	wav, err := sineWAV(48000, 1, 300, 440, 0.3)
	if err != nil {
		return err
	}
	if _, status, err := harness.createSource(ctx, "source_retention_01", "retention take", wav); err != nil || status != 201 {
		return fmt.Errorf("source setup failed: status=%d err=%v", status, err)
	}
	spans := []domain.TimelineSpan{
		{SourceID: "source_retention_01", SourceStartFrame: 0, SourceEndFrame: 14400, GainDB: 0, CrossfadeFrames: 0},
	}
	plan, status, err := harness.createTimeline(ctx, "timeline_retention_01", "retention assembly", 48000, 1, -1, spans)
	if err != nil || status != 201 {
		return fmt.Errorf("timeline setup failed: status=%d err=%v", status, err)
	}

	// A completed render carries a durable retention expiry.
	job, status, err := harness.createRender(ctx, "render_retention_01", plan.ID)
	if err != nil {
		return err
	}
	if status != 201 || job.State != domain.RenderCompleted {
		return fmt.Errorf("render returned status=%d state=%s", status, job.State)
	}
	if job.Retention == nil || job.Retention.ExpiresAt == nil {
		return fmt.Errorf("completed job carries no retention expiry: %+v", job.Retention)
	}
	expiresAt := *job.Retention.ExpiresAt

	receipt, status, err := harness.getReceipt(ctx, job.ID)
	if err != nil || status != 200 {
		return fmt.Errorf("receipt before expiry failed: status=%d err=%v", status, err)
	}
	audioBytes, status, err := harness.getOutput(ctx, job.ID)
	if err != nil || status != 200 {
		return fmt.Errorf("output before expiry failed: status=%d err=%v", status, err)
	}
	if digestOf(audioBytes) != receipt.Output.Digest {
		return fmt.Errorf("downloaded bytes do not match the receipt digest")
	}

	// Repeated downloads must not push the expiry out.
	for i := 0; i < 3; i++ {
		if _, status, err := harness.getOutput(ctx, job.ID); err != nil || status != 200 {
			return fmt.Errorf("repeat download %d failed: status=%d err=%v", i, status, err)
		}
	}
	reread, status, err := harness.getRender(ctx, job.ID)
	if err != nil || status != 200 {
		return fmt.Errorf("job read-back failed: status=%d err=%v", status, err)
	}
	if reread.Retention == nil || reread.Retention.ExpiresAt == nil || !reread.Retention.ExpiresAt.Equal(expiresAt) {
		return fmt.Errorf("downloads changed the retention expiry: want %s got %+v", expiresAt, reread.Retention)
	}

	// The sweep at expiry reclaims the artifact.
	summary, err := harness.reclaim(ctx, expiresAt.Add(time.Second))
	if err != nil {
		return fmt.Errorf("retention sweep failed: %w", err)
	}
	if summary.Expired != 1 {
		return fmt.Errorf("sweep reclaimed %d artifacts, want 1", summary.Expired)
	}
	if err := expectReclaimed(ctx, harness, job.ID, domain.ReclaimReasonRetentionExpired, receipt.Output.Digest); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(harness.renderDir(job.ID), "output.wav")); !os.IsNotExist(err) {
		return fmt.Errorf("reclaimed output file still occupies storage: stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(harness.renderDir(job.ID), "receipt.json")); err != nil {
		return fmt.Errorf("receipt history was removed with the artifact: %v", err)
	}

	// After a process restart the three endpoints still tell the same story.
	if err := harness.restart(); err != nil {
		return fmt.Errorf("restart failed: %w", err)
	}
	if err := expectReclaimed(ctx, harness, job.ID, domain.ReclaimReasonRetentionExpired, receipt.Output.Digest); err != nil {
		return fmt.Errorf("after restart: %w", err)
	}

	// A download racing the sweep either completes with the full original
	// bytes or is refused cleanly; it is never truncated.
	racing, status, err := harness.createRender(ctx, "render_retention_02", plan.ID)
	if err != nil || status != 201 || racing.State != domain.RenderCompleted {
		return fmt.Errorf("second render failed: status=%d err=%v", status, err)
	}
	racingReceipt, status, err := harness.getReceipt(ctx, racing.ID)
	if err != nil || status != 200 {
		return fmt.Errorf("second receipt failed: status=%d err=%v", status, err)
	}
	if err := raceDownloadsAgainstSweep(ctx, harness, racing.ID, racingReceipt.Output.Digest); err != nil {
		return err
	}
	if err := expectReclaimed(ctx, harness, racing.ID, domain.ReclaimReasonRetentionExpired, racingReceipt.Output.Digest); err != nil {
		return err
	}

	// An operator deleting the directory by hand is reconciled into the same
	// honest reclaimed state instead of a job that keeps claiming the output
	// is downloadable.
	orphaned, status, err := harness.createRender(ctx, "render_retention_03", plan.ID)
	if err != nil || status != 201 || orphaned.State != domain.RenderCompleted {
		return fmt.Errorf("third render failed: status=%d err=%v", status, err)
	}
	if err := os.Remove(filepath.Join(harness.renderDir(orphaned.ID), "output.wav")); err != nil {
		return fmt.Errorf("manual cleanup simulation failed: %w", err)
	}
	summary, err = harness.reclaim(ctx, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("reconciliation sweep failed: %w", err)
	}
	if summary.Missing != 1 {
		return fmt.Errorf("sweep reconciled %d missing outputs, want 1", summary.Missing)
	}
	orphanReceipt, status, err := harness.getReceipt(ctx, orphaned.ID)
	if err != nil || status != 200 {
		return fmt.Errorf("orphaned receipt failed: status=%d err=%v", status, err)
	}
	if err := expectReclaimed(ctx, harness, orphaned.ID, domain.ReclaimReasonOutputMissing, orphanReceipt.Output.Digest); err != nil {
		return err
	}
	return nil
}

// expectReclaimed asserts that the job query, the receipt, and the download
// endpoint agree the artifact is reclaimed while the delivery history stays
// readable.
func expectReclaimed(ctx context.Context, harness *Harness, renderID, reason, wantDigest string) error {
	job, status, err := harness.getRender(ctx, renderID)
	if err != nil || status != 200 {
		return fmt.Errorf("job query failed: status=%d err=%v", status, err)
	}
	if job.State != domain.RenderCompleted {
		return fmt.Errorf("reclaimed job changed its terminal state to %s", job.State)
	}
	if job.Retention == nil || job.Retention.ReclaimedAt == nil {
		return fmt.Errorf("job does not record the reclamation: %+v", job.Retention)
	}
	if job.Retention.ReclaimReason != reason {
		return fmt.Errorf("reclaim reason %q, want %q", job.Retention.ReclaimReason, reason)
	}
	if job.Output == nil || job.Output.Digest != wantDigest {
		return fmt.Errorf("job lost the delivered output metadata: %+v", job.Output)
	}

	body, status, err := harness.getReceiptRaw(ctx, renderID)
	if err != nil || status != 200 {
		return fmt.Errorf("receipt after reclaim failed: status=%d err=%v", status, err)
	}
	var view receiptView
	if err := decodeBody(body, &view); err != nil {
		return err
	}
	if view.Output.Digest != wantDigest {
		return fmt.Errorf("receipt no longer describes the delivered audio")
	}
	if view.Retention == nil || view.Retention.ReclaimedAt == nil {
		return fmt.Errorf("receipt response does not explain the reclamation: %+v", view.Retention)
	}

	outputBody, status, err := harness.getOutputRaw(ctx, renderID)
	if err != nil {
		return fmt.Errorf("output after reclaim failed: %w", err)
	}
	if status != 410 || failureCode(outputBody) != "render_output_reclaimed" {
		return fmt.Errorf("output after reclaim returned status=%d code=%s, want 410 render_output_reclaimed", status, failureCode(outputBody))
	}
	return nil
}

// raceDownloadsAgainstSweep runs a burst of downloads concurrently with the
// retention sweep and asserts every one of them either delivers the complete
// original file or is refused with the stable reclaimed error.
func raceDownloadsAgainstSweep(ctx context.Context, harness *Harness, renderID, wantDigest string) error {
	const downloaders = 16
	start := make(chan struct{})
	outcomes := make(chan error, downloaders)
	var wg sync.WaitGroup
	for i := 0; i < downloaders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			body, status, err := harness.getOutputRaw(ctx, renderID)
			switch {
			case err != nil:
				outcomes <- fmt.Errorf("download transport error: %w", err)
			case status == 200:
				if digestOf(body) != wantDigest {
					outcomes <- fmt.Errorf("download delivered bytes that do not match the receipt digest")
					return
				}
				outcomes <- nil
			case status == 410 && failureCode(body) == "render_output_reclaimed":
				outcomes <- nil
			default:
				outcomes <- fmt.Errorf("download returned status=%d code=%s", status, failureCode(body))
			}
		}()
	}
	sweepDone := make(chan error, 1)
	go func() {
		_, err := harness.reclaim(ctx, time.Now().UTC().Add(2*time.Hour))
		sweepDone <- err
	}()
	close(start)
	wg.Wait()
	close(outcomes)
	if err := <-sweepDone; err != nil {
		return fmt.Errorf("racing sweep failed: %w", err)
	}
	for outcome := range outcomes {
		if outcome != nil {
			return outcome
		}
	}
	// New downloads after the reclamation must keep being refused, so the
	// expired artifact cannot be pinned in storage by fresh traffic.
	for i := 0; i < 3; i++ {
		body, status, err := harness.getOutputRaw(ctx, renderID)
		if err != nil {
			return fmt.Errorf("post-reclaim download failed: %w", err)
		}
		if status != 410 || failureCode(body) != "render_output_reclaimed" {
			return fmt.Errorf("post-reclaim download returned status=%d code=%s", status, failureCode(body))
		}
	}
	if _, err := os.Stat(filepath.Join(harness.renderDir(renderID), "output.wav")); !os.IsNotExist(err) {
		return fmt.Errorf("expired output still occupies storage after the sweep: stat err=%v", err)
	}
	return nil
}

func digestOf(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
