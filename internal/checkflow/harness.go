package checkflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"voice-timeline-renderer/internal/httpapi"
	"voice-timeline-renderer/internal/service"
	"voice-timeline-renderer/internal/storage"
)

type Harness struct {
	baseURL string
	client  *http.Client
	tempDir string
	config  service.Config
	service *service.Service
	server  *httptest.Server
}

func NewHarness() (*Harness, error) {
	return NewHarnessWithConfig(service.Config{})
}

func NewHarnessWithConfig(config service.Config) (*Harness, error) {
	tempDir, err := os.MkdirTemp("", "voice-timeline-check-*")
	if err != nil {
		return nil, err
	}
	harness := &Harness{
		client:  &http.Client{Timeout: 45 * time.Second},
		tempDir: tempDir,
		config:  config,
	}
	if err := harness.start(); err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, err
	}
	return harness, nil
}

// start builds a fresh repository, service, and HTTP server over the harness
// data directory, exactly as a process launch would.
func (harness *Harness) start() error {
	repo, err := storage.NewRepository(harness.tempDir)
	if err != nil {
		return err
	}
	serviceLayer := service.New(repo, harness.config)
	httpServer := httpapi.New(serviceLayer, httpapi.Options{
		MaxSourceRequestBytes: 48 << 20,
		MaxJSONBytes:          2 << 20,
	})
	server := httptest.NewServer(httpServer.Handler())
	harness.baseURL = server.URL
	harness.service = serviceLayer
	harness.server = server
	return nil
}

// restart simulates a process exit and relaunch over the same data directory.
func (harness *Harness) restart() error {
	harness.server.Close()
	return harness.start()
}

// reclaim runs one retention sweep against the live service, as the
// background sweeper would at the given moment.
func (harness *Harness) reclaim(ctx context.Context, now time.Time) (service.ReclaimSummary, error) {
	return harness.service.ReclaimExpiredRenders(ctx, now)
}

// renderDir returns the on-disk artifact directory of one render.
func (harness *Harness) renderDir(renderID string) string {
	return harness.tempDir + string(os.PathSeparator) + "renders" + string(os.PathSeparator) + renderID
}

func (harness *Harness) Close() {
	harness.server.Close()
	_ = os.RemoveAll(harness.tempDir)
}

func (harness *Harness) requestJSON(
	ctx context.Context,
	method string,
	path string,
	payload any,
) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, harness.baseURL+path, body)
	if err != nil {
		return 0, nil, err
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := harness.client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	result, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, nil, err
	}
	return response.StatusCode, result, nil
}

func decodeBody(body []byte, target any) error {
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode response %q: %w", string(body), err)
	}
	return nil
}
