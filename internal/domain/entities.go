package domain

import "time"

type SourceAsset struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Digest        string    `json:"digest"`
	ByteCount     int64     `json:"byte_count"`
	SampleRate    int       `json:"sample_rate"`
	Channels      int       `json:"channels"`
	BitsPerSample int       `json:"bits_per_sample"`
	FrameCount    int64     `json:"frame_count"`
	DurationMS    int64     `json:"duration_ms"`
	PeakDBFS      float64   `json:"peak_dbfs"`
	RMSDBFS       float64   `json:"rms_dbfs"`
	CreatedAt     time.Time `json:"created_at"`
}

type TimelineSpan struct {
	SourceID         string  `json:"source_id"`
	SourceStartFrame int64   `json:"source_start_frame"`
	SourceEndFrame   int64   `json:"source_end_frame"`
	GainDB           float64 `json:"gain_db"`
	CrossfadeFrames  int64   `json:"crossfade_frames"`
}

type Timeline struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	SampleRate     int            `json:"sample_rate"`
	OutputChannels int            `json:"output_channels"`
	TargetPeakDBFS float64        `json:"target_peak_dbfs"`
	Spans          []TimelineSpan `json:"spans"`
	State          string         `json:"state"`
	OutputFrames   int64          `json:"output_frames"`
	CreatedAt      time.Time      `json:"created_at"`
}

type RenderState string

const (
	RenderQueued     RenderState = "queued"
	RenderRendering  RenderState = "rendering"
	RenderFinalizing RenderState = "finalizing"
	RenderCompleted  RenderState = "completed"
	RenderFailed     RenderState = "failed"
)

// Reclaim reasons recorded on a job when its output artifact leaves storage.
const (
	ReclaimReasonRetentionExpired = "retention_expired"
	ReclaimReasonOutputMissing    = "output_missing"
)

// RenderRetention is the durable lifecycle record of a completed render's
// output artifact. ExpiresAt is stamped when the job completes; ReclaimedAt
// and ReclaimReason are stamped when the artifact leaves storage, whether by
// retention expiry or by reconciling an externally removed file.
type RenderRetention struct {
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	ReclaimedAt   *time.Time `json:"reclaimed_at,omitempty"`
	ReclaimReason string     `json:"reclaim_reason,omitempty"`
}

type OutputAsset struct {
	Digest        string  `json:"digest"`
	ByteCount     int64   `json:"byte_count"`
	SampleRate    int     `json:"sample_rate"`
	Channels      int     `json:"channels"`
	BitsPerSample int     `json:"bits_per_sample"`
	FrameCount    int64   `json:"frame_count"`
	DurationMS    int64   `json:"duration_ms"`
	PeakDBFS      float64 `json:"peak_dbfs"`
	RMSDBFS       float64 `json:"rms_dbfs"`
}

type RenderJob struct {
	ID            string           `json:"id"`
	TimelineID    string           `json:"timeline_id"`
	State         RenderState      `json:"state"`
	Error         *ErrorDetail     `json:"error,omitempty"`
	Output        *OutputAsset     `json:"output,omitempty"`
	ReceiptDigest string           `json:"receipt_digest,omitempty"`
	Retention     *RenderRetention `json:"retention,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

// Reclaimed reports whether the render output artifact has left storage. The
// job record and receipt remain as the durable history of the delivery.
func (job *RenderJob) Reclaimed() bool {
	return job.Retention != nil && job.Retention.ReclaimedAt != nil
}

// MarkReclaimed records that the output artifact left storage at the given
// time. It is not a state transition: the terminal state and UpdatedAt stay
// untouched so the job keeps describing the original delivery.
func (job *RenderJob) MarkReclaimed(at time.Time, reason string) {
	if job.Retention == nil {
		job.Retention = &RenderRetention{}
	}
	stamp := at.UTC()
	job.Retention.ReclaimedAt = &stamp
	job.Retention.ReclaimReason = reason
}

type ReceiptSpan struct {
	Index            int     `json:"index"`
	SourceID         string  `json:"source_id"`
	SourceDigest     string  `json:"source_digest"`
	SourceStartFrame int64   `json:"source_start_frame"`
	SourceEndFrame   int64   `json:"source_end_frame"`
	OutputStartFrame int64   `json:"output_start_frame"`
	OutputEndFrame   int64   `json:"output_end_frame"`
	OutputFrames     int64   `json:"output_frames"`
	CrossfadeFrames  int64   `json:"crossfade_frames"`
	GainDB           float64 `json:"gain_db"`
	ChannelAction    string  `json:"channel_action"`
}

type NormalizationReceipt struct {
	RequestedPeakDBFS float64 `json:"requested_peak_dbfs"`
	BeforePeakDBFS    float64 `json:"before_peak_dbfs"`
	AppliedGainDB     float64 `json:"applied_gain_db"`
	OutputPeakDBFS    float64 `json:"output_peak_dbfs"`
}

type RenderReceipt struct {
	SchemaVersion int                  `json:"schema_version"`
	RenderID      string               `json:"render_id"`
	TimelineID    string               `json:"timeline_id"`
	Output        OutputAsset          `json:"output"`
	Normalization NormalizationReceipt `json:"normalization"`
	Spans         []ReceiptSpan        `json:"spans"`
	Warnings      []string             `json:"warnings"`
	CreatedAt     time.Time            `json:"created_at"`
}

func ValidRenderTransition(from, to RenderState) bool {
	switch from {
	case RenderQueued:
		return to == RenderRendering || to == RenderFailed
	case RenderRendering:
		return to == RenderFinalizing || to == RenderFailed
	case RenderFinalizing:
		return to == RenderCompleted || to == RenderFailed
	default:
		return false
	}
}

func (job *RenderJob) Transition(to RenderState, detail *ErrorDetail, now time.Time) error {
	if job.State == to {
		return nil
	}
	if !ValidRenderTransition(job.State, to) {
		return E(
			KindConflict,
			"invalid_state_transition",
			"state",
			"render job cannot move from "+string(job.State)+" to "+string(to),
		)
	}
	if to == RenderFailed && detail == nil {
		return E(KindInternal, "missing_failure_detail", "error", "a failed render requires an error detail")
	}
	if to != RenderFailed {
		detail = nil
	}
	job.State = to
	job.Error = detail
	job.UpdatedAt = now.UTC()
	return nil
}
