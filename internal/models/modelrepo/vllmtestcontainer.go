package modelrepo

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	vllmPort       = "8000/tcp"
	vllmHealthPath = "/health"
	vllmModelsPath = "/v1/models"
	// Must exceed the largest output budget a client requests (the prompt client
	// asks for max_tokens = context length, up to 2048) plus the prompt itself —
	// otherwise vLLM 400s with "maximum context length" when output+input > limit.
	defaultMaxModelLen = "4096"
	defaultModel       = "HuggingFaceTB/SmolLM2-360M-Instruct"
	defaultTag         = "latest"
	startupTimeout     = 8 * time.Minute
	pollInterval       = 10 * time.Second
	readinessRetries   = 15
)

// SetupVLLMLocalInstance creates a vLLM container for testing.
func SetupVLLMLocalInstance(ctx context.Context, model string, tag string, toolParser string) (string, testcontainers.Container, func(), error) {
	if model == "" {
		model = defaultModel
	}
	if tag == "" {
		tag = defaultTag
	}

	cleanup := func() {}
	// Memory-bound the CPU backend: vLLM-CPU reserves gpu-memory-utilization *
	// total_RAM at startup and aborts if that exceeds what's free (default 0.92
	// ~ all RAM), so 0.3 keeps the target small. max-model-len must be a flag
	// (the MAX_MODEL_LEN env is ignored by vLLM).
	cmd := []string{
		"--model", model,
		"--max-model-len", defaultMaxModelLen,
		"--max-num-seqs", "1",
		"--gpu-memory-utilization", "0.3",
		"--enforce-eager",
	}
	if toolParser != "" && toolParser != "none" {
		cmd = append(cmd, "--enable-auto-tool-choice", "--tool-call-parser", toolParser)
	}
	req := testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: "openeuler/vllm-cpu:" + tag,
			Env: map[string]string{
				"MODEL":                  model,
				"VLLM_CPU_KVCACHE_SPACE": "1",
			},
			Cmd:          cmd,
			Privileged:   true,
			ExposedPorts: []string{vllmPort},
			WaitingFor: wait.ForHTTP(vllmHealthPath).
				WithPort(vllmPort).
				WithStartupTimeout(startupTimeout).
				WithPollInterval(pollInterval),
			AlwaysPullImage: true,
		},
		Started: true,
	}

	container, err := testcontainers.GenericContainer(ctx, req)
	if err != nil {
		return "", nil, cleanup, fmt.Errorf("failed to create vLLM container: %w", err)
	}

	cleanup = func() {
		if err := container.Terminate(context.Background()); err != nil {
			log.Printf("failed to terminate vLLM container: %v", err)
		}
	}

	host, err := container.Host(ctx)
	if err != nil {
		return "", nil, cleanup, fmt.Errorf("failed to get vLLM host: %w", err)
	}

	mappedPort, err := container.MappedPort(ctx, vllmPort)
	if err != nil {
		return "", nil, cleanup, fmt.Errorf("failed to get vLLM port: %w", err)
	}

	apiBase := fmt.Sprintf("http://%s:%s", host, mappedPort.Port())

	if err := waitForModelsEndpoint(ctx, apiBase); err != nil {
		return "", nil, cleanup, fmt.Errorf("vLLM server failed to become fully ready: %w", err)
	}

	return apiBase, container, cleanup, nil
}

// waitForModelsEndpoint polls the /v1/models endpoint to ensure the model is fully loaded.
func waitForModelsEndpoint(ctx context.Context, apiBase string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	modelsURL := apiBase + vllmModelsPath

	for i := range readinessRetries {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create models request: %w", err)
		}

		resp, err := client.Do(req)
		if err == nil {
			defer func() {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}()

			if resp.StatusCode == http.StatusOK {
				log.Printf("vLLM instance is ready at %s", apiBase)
				bodyBytes, err := io.ReadAll(resp.Body)
				if err != nil {
					log.Printf("failed to read models response body: %v", err)
				} else {
					log.Printf("vLLM /v1/models response: %s", string(bodyBytes))
				}
				resp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

				log.Printf("vLLM instance is ready at %s", apiBase)
				return nil
			}
			log.Printf("vLLM models check returned status %d (attempt %d/%d)", resp.StatusCode, i+1, readinessRetries)
		} else {
			log.Printf("vLLM models check failed (attempt %d/%d): %v", i+1, readinessRetries, err)
		}

		select {
		case <-time.After(pollInterval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return fmt.Errorf("timed out after %d retries", readinessRetries)
}
