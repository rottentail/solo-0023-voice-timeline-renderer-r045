package service

import (
	"context"
	"sync"
	"time"

	"voice-timeline-renderer/internal/domain"
)

type Repository interface {
	SaveSource(domain.SourceAsset, []byte) error
	GetSourceMeta(string) (domain.SourceAsset, error)
	GetSource(string) (domain.SourceAsset, []byte, error)
	SaveTimeline(domain.Timeline) error
	GetTimeline(string) (domain.Timeline, error)
	SaveJob(domain.RenderJob) error
	GetJob(string) (domain.RenderJob, error)
	ListJobs() ([]domain.RenderJob, error)
	CommitRenderArtifacts(string, []byte, domain.RenderReceipt) error
	GetRenderReceipt(string) (domain.RenderReceipt, error)
	GetRenderDownload(string) (domain.RenderJob, []byte, domain.RenderReceipt, error)
	RenderOutputExists(string) (bool, error)
	DeleteRenderOutput(string) error
}

type Config struct {
	MaxSourceBytes  int64
	MaxSpans        int
	MaxOutputFrames int64
	// RenderRetention is how long a completed render's output artifact is
	// kept before the retention sweep reclaims it. Non-positive values fall
	// back to the default.
	RenderRetention time.Duration
}

func DefaultConfig() Config {
	return Config{
		MaxSourceBytes:  32 << 20,
		MaxSpans:        64,
		MaxOutputFrames: 48000 * 180,
		RenderRetention: 7 * 24 * time.Hour,
	}
}

type Service struct {
	repo   Repository
	config Config
	mu     sync.Mutex
}

func New(repo Repository, config Config) *Service {
	defaults := DefaultConfig()
	if config.MaxSourceBytes <= 0 {
		config.MaxSourceBytes = defaults.MaxSourceBytes
	}
	if config.MaxSpans <= 0 {
		config.MaxSpans = defaults.MaxSpans
	}
	if config.MaxOutputFrames <= 0 {
		config.MaxOutputFrames = defaults.MaxOutputFrames
	}
	if config.RenderRetention <= 0 {
		config.RenderRetention = defaults.RenderRetention
	}
	return &Service{repo: repo, config: config}
}

type SourceWriteRequest struct {
	Name      string `json:"name"`
	WAVBase64 string `json:"wav_base64"`
}

type TimelineWriteRequest struct {
	Name           string                `json:"name"`
	SampleRate     int                   `json:"sample_rate"`
	OutputChannels int                   `json:"output_channels"`
	TargetPeakDBFS float64               `json:"target_peak_dbfs"`
	Spans          []domain.TimelineSpan `json:"spans"`
}

type RenderWriteRequest struct {
	TimelineID string `json:"timeline_id"`
}

type RenderResult struct {
	Job domain.RenderJob
	WAV []byte
}

func (s *Service) GetSource(ctx context.Context, id string) (domain.SourceAsset, error) {
	if err := ctx.Err(); err != nil {
		return domain.SourceAsset{}, err
	}
	return s.repo.GetSourceMeta(id)
}

func (s *Service) GetTimeline(ctx context.Context, id string) (domain.Timeline, error) {
	if err := ctx.Err(); err != nil {
		return domain.Timeline{}, err
	}
	return s.repo.GetTimeline(id)
}

func (s *Service) GetRender(ctx context.Context, id string) (domain.RenderJob, error) {
	if err := ctx.Err(); err != nil {
		return domain.RenderJob{}, err
	}
	return s.repo.GetJob(id)
}

// ReceiptView is the receipt document annotated with the job's retention
// record, so the receipt endpoint explains whether the described artifact can
// still be downloaded. The stored receipt file itself stays immutable.
type ReceiptView struct {
	domain.RenderReceipt
	Retention *domain.RenderRetention `json:"retention,omitempty"`
}

func (s *Service) GetReceipt(ctx context.Context, id string) (ReceiptView, error) {
	if err := ctx.Err(); err != nil {
		return ReceiptView{}, err
	}
	job, err := s.repo.GetJob(id)
	if err != nil {
		return ReceiptView{}, err
	}
	if job.State != domain.RenderCompleted {
		return ReceiptView{}, domain.E(domain.KindUnready, "render_not_completed", "render_id", "render receipt is available only after completion")
	}
	receipt, err := s.repo.GetRenderReceipt(id)
	if err != nil {
		if appErr := domain.AsAppError(err); appErr.Kind == domain.KindUnready {
			// A completed job whose receipt document itself vanished can only
			// be reported honestly as no longer stored.
			return ReceiptView{}, reclaimedError()
		}
		return ReceiptView{}, err
	}
	return ReceiptView{RenderReceipt: receipt, Retention: job.Retention}, nil
}

func (s *Service) GetOutput(ctx context.Context, id string) ([]byte, domain.RenderReceipt, error) {
	if err := ctx.Err(); err != nil {
		return nil, domain.RenderReceipt{}, err
	}
	// The job record and the output bytes are read under one storage lock, so
	// a download admitted here assembles the complete original file even when
	// a retention sweep is waiting to reclaim it.
	job, payload, receipt, err := s.repo.GetRenderDownload(id)
	if err != nil {
		if appErr := domain.AsAppError(err); appErr.Kind == domain.KindNotFound {
			return nil, domain.RenderReceipt{}, err
		}
		if job.State == domain.RenderCompleted {
			// The artifact file is gone; the job record is the source of
			// truth for why (reclaimed, or externally removed and about to be
			// reconciled by the sweep).
			return nil, domain.RenderReceipt{}, reclaimedError()
		}
		return nil, domain.RenderReceipt{}, err
	}
	if job.State != domain.RenderCompleted {
		return nil, domain.RenderReceipt{}, domain.E(domain.KindUnready, "render_not_completed", "render_id", "render output is available only after completion")
	}
	if job.Reclaimed() {
		// Marked reclaimed but the file is still on disk: the sweep has
		// recorded the reclamation and is about to delete the bytes. The job
		// record wins, so the endpoints never disagree.
		return nil, domain.RenderReceipt{}, reclaimedError()
	}
	return payload, receipt, nil
}

func reclaimedError() *domain.AppError {
	return domain.E(domain.KindGone, "render_output_reclaimed", "render_id", "render output was reclaimed and is no longer downloadable; the job record and receipt remain available")
}
