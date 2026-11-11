package checkflow

import (
	"context"
	"encoding/base64"
	"fmt"

	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/service"
)

type failureEnvelope struct {
	Error domain.ErrorDetail `json:"error"`
	Job   *domain.RenderJob  `json:"job,omitempty"`
}

func (harness *Harness) createSource(
	ctx context.Context,
	id string,
	name string,
	wav []byte,
) (domain.SourceAsset, int, error) {
	status, body, err := harness.requestJSON(ctx, "PUT", "/v1/sources/"+id, service.SourceWriteRequest{
		Name:      name,
		WAVBase64: base64.StdEncoding.EncodeToString(wav),
	})
	if err != nil {
		return domain.SourceAsset{}, status, err
	}
	var asset domain.SourceAsset
	if status == 200 || status == 201 {
		if err := decodeBody(body, &asset); err != nil {
			return domain.SourceAsset{}, status, err
		}
	}
	return asset, status, nil
}

func (harness *Harness) getSource(
	ctx context.Context,
	id string,
) (domain.SourceAsset, int, error) {
	status, body, err := harness.requestJSON(ctx, "GET", "/v1/sources/"+id, nil)
	if err != nil {
		return domain.SourceAsset{}, status, err
	}
	var asset domain.SourceAsset
	if status == 200 {
		if err := decodeBody(body, &asset); err != nil {
			return domain.SourceAsset{}, status, err
		}
	}
	return asset, status, nil
}

func (harness *Harness) createTimeline(
	ctx context.Context,
	id string,
	name string,
	sampleRate int,
	channels int,
	targetPeak float64,
	spans []domain.TimelineSpan,
) (domain.Timeline, int, error) {
	status, body, err := harness.requestJSON(ctx, "PUT", "/v1/timelines/"+id, service.TimelineWriteRequest{
		Name:           name,
		SampleRate:     sampleRate,
		OutputChannels: channels,
		TargetPeakDBFS: targetPeak,
		Spans:          spans,
	})
	if err != nil {
		return domain.Timeline{}, status, err
	}
	var plan domain.Timeline
	if status == 200 || status == 201 {
		if err := decodeBody(body, &plan); err != nil {
			return domain.Timeline{}, status, err
		}
	}
	return plan, status, nil
}

func (harness *Harness) getTimeline(
	ctx context.Context,
	id string,
) (domain.Timeline, int, error) {
	status, body, err := harness.requestJSON(ctx, "GET", "/v1/timelines/"+id, nil)
	if err != nil {
		return domain.Timeline{}, status, err
	}
	var plan domain.Timeline
	if status == 200 {
		if err := decodeBody(body, &plan); err != nil {
			return domain.Timeline{}, status, err
		}
	}
	return plan, status, nil
}

func (harness *Harness) createRender(
	ctx context.Context,
	id string,
	timelineID string,
) (domain.RenderJob, int, error) {
	status, body, err := harness.requestJSON(ctx, "PUT", "/v1/renders/"+id, service.RenderWriteRequest{
		TimelineID: timelineID,
	})
	if err != nil {
		return domain.RenderJob{}, status, err
	}
	var job domain.RenderJob
	if status == 200 || status == 201 {
		if err := decodeBody(body, &job); err != nil {
			return domain.RenderJob{}, status, err
		}
	}
	return job, status, nil
}

func (harness *Harness) getRender(
	ctx context.Context,
	id string,
) (domain.RenderJob, int, error) {
	status, body, err := harness.requestJSON(ctx, "GET", "/v1/renders/"+id, nil)
	if err != nil {
		return domain.RenderJob{}, status, err
	}
	var job domain.RenderJob
	if status == 200 {
		if err := decodeBody(body, &job); err != nil {
			return domain.RenderJob{}, status, err
		}
	}
	return job, status, nil
}

func (harness *Harness) getReceipt(
	ctx context.Context,
	id string,
) (domain.RenderReceipt, int, error) {
	status, body, err := harness.requestJSON(ctx, "GET", "/v1/renders/"+id+"/receipt", nil)
	if err != nil {
		return domain.RenderReceipt{}, status, err
	}
	var receipt domain.RenderReceipt
	if status == 200 {
		if err := decodeBody(body, &receipt); err != nil {
			return domain.RenderReceipt{}, status, err
		}
	}
	return receipt, status, nil
}

func (harness *Harness) getOutput(
	ctx context.Context,
	id string,
) ([]byte, int, error) {
	status, body, err := harness.requestJSON(ctx, "GET", "/v1/renders/"+id+"/output", nil)
	if err != nil {
		return nil, status, err
	}
	if status != 200 {
		var envelope failureEnvelope
		_ = decodeBody(body, &envelope)
		return nil, status, fmt.Errorf("unexpected output status %d with error %s", status, envelope.Error.Code)
	}
	return body, status, nil
}

// getOutputRaw returns the output response without interpreting the status,
// so checks can assert gone and other non-200 outcomes.
func (harness *Harness) getOutputRaw(
	ctx context.Context,
	id string,
) ([]byte, int, error) {
	status, body, err := harness.requestJSON(ctx, "GET", "/v1/renders/"+id+"/output", nil)
	if err != nil {
		return nil, status, err
	}
	return body, status, nil
}

// getReceiptRaw returns the receipt response body and status without
// interpreting them.
func (harness *Harness) getReceiptRaw(
	ctx context.Context,
	id string,
) ([]byte, int, error) {
	status, body, err := harness.requestJSON(ctx, "GET", "/v1/renders/"+id+"/receipt", nil)
	if err != nil {
		return nil, status, err
	}
	return body, status, nil
}

func failureCode(body []byte) string {
	var envelope failureEnvelope
	if err := decodeBody(body, &envelope); err != nil {
		return ""
	}
	return envelope.Error.Code
}
