package storage

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"voice-timeline-renderer/internal/domain"
)

func (repo *Repository) SaveJob(job domain.RenderJob) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	path := repo.path("jobs", job.ID+".json")
	if err := writeJSONAtomic(path, job, 0o640); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot write render job", err)
	}
	return nil
}

func (repo *Repository) GetJob(id string) (domain.RenderJob, error) {
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	return repo.getJobLocked(id)
}

func (repo *Repository) getJobLocked(id string) (domain.RenderJob, error) {
	var job domain.RenderJob
	if err := readJSON(repo.path("jobs", id+".json"), &job); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.RenderJob{}, domain.E(domain.KindNotFound, "render_not_found", "render_id", "render job does not exist")
		}
		return domain.RenderJob{}, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read render job", err)
	}
	return job, nil
}

// ListJobs returns every persisted render job ordered by identifier. Records
// that cannot be decoded are skipped and reported through the returned error
// so a retention sweep can still process the healthy majority.
func (repo *Repository) ListJobs() ([]domain.RenderJob, error) {
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	entries, err := os.ReadDir(repo.path("jobs"))
	if err != nil {
		return nil, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot list render jobs", err)
	}
	jobs := make([]domain.RenderJob, 0, len(entries))
	var failures []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var job domain.RenderJob
		if err := readJSON(repo.path("jobs", entry.Name()), &job); err != nil {
			failures = append(failures, err)
			continue
		}
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	return jobs, errors.Join(failures...)
}

func (repo *Repository) CommitRenderArtifacts(id string, wav []byte, receipt domain.RenderReceipt) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	final := repo.path("renders", id)
	if _, err := os.Stat(final); err == nil {
		return domain.E(domain.KindConflict, "render_conflict", "render_id", "render artifacts already exist")
	} else if !errors.Is(err, os.ErrNotExist) {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot inspect render destination", err)
	}
	temp, err := os.MkdirTemp(repo.path("tmp"), "render-*")
	if err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot create render staging directory", err)
	}
	defer func() {
		_ = os.RemoveAll(temp)
	}()
	if err := writeBytes(filepath.Join(temp, "output.wav"), wav, 0o640); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot write render output", err)
	}
	if err := writeJSONAtomic(filepath.Join(temp, "receipt.json"), receipt, 0o640); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot write render receipt", err)
	}
	if err := os.Rename(temp, final); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot commit render artifacts", err)
	}
	if err := syncDirectory(repo.path("renders")); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot sync render index", err)
	}
	return nil
}

// GetRenderReceipt reads only the receipt document. The receipt is retained
// after output reclamation, so it remains the durable history of the render.
func (repo *Repository) GetRenderReceipt(id string) (domain.RenderReceipt, error) {
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	return repo.getReceiptLocked(id)
}

func (repo *Repository) getReceiptLocked(id string) (domain.RenderReceipt, error) {
	var receipt domain.RenderReceipt
	if err := readJSON(repo.path("renders", id, "receipt.json"), &receipt); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.RenderReceipt{}, domain.E(domain.KindUnready, "render_not_completed", "render_id", "render receipt is not available")
		}
		return domain.RenderReceipt{}, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read render receipt", err)
	}
	return receipt, nil
}

// GetRenderDownload reads the job record, receipt, and output payload under a
// single read lock. A retention sweep can only delete the output file after
// this snapshot is fully in memory, so an admitted download always assembles
// the complete original bytes and is never truncated mid-read.
func (repo *Repository) GetRenderDownload(id string) (domain.RenderJob, []byte, domain.RenderReceipt, error) {
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	job, err := repo.getJobLocked(id)
	if err != nil {
		return domain.RenderJob{}, nil, domain.RenderReceipt{}, err
	}
	receipt, err := repo.getReceiptLocked(id)
	if err != nil {
		return job, nil, domain.RenderReceipt{}, err
	}
	payload, err := os.ReadFile(repo.path("renders", id, "output.wav"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return job, nil, receipt, domain.E(domain.KindUnready, "render_not_completed", "render_id", "render output is not available")
		}
		return job, nil, receipt, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read render output", err)
	}
	return job, payload, receipt, nil
}

// RenderOutputExists reports whether the render output file is still stored.
func (repo *Repository) RenderOutputExists(id string) (bool, error) {
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	_, err := os.Stat(repo.path("renders", id, "output.wav"))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot inspect render output", err)
}

// DeleteRenderOutput removes only the render output file. The receipt and job
// records stay behind as the durable history of the delivery. The deletion is
// idempotent so an interrupted sweep can be safely retried.
func (repo *Repository) DeleteRenderOutput(id string) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if err := os.Remove(repo.path("renders", id, "output.wav")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot delete render output", err)
	}
	if err := syncDirectory(repo.path("renders", id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot sync render directory", err)
	}
	return nil
}
