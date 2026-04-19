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
	vllmPort           = "8000/tcp"
	vllmHealthPath     = "/health"
	vllmModelsPath     = "/v1/models"
	defaultMaxModelLen = "512"
	defaultModel       = "HuggingFaceTB/SmolLM2-360M-Instruct"
	defaultTag         = "latest"
	startupTimeout     = 8 * time.Minute
	pollInterval       = 10 * time.Second
	readinessRetries   = 15
)

// SetupVLLMLocalInstance creates a vLLM container for testing.
func SetupVLLMLocalInstance(ctx context.Context, model string, tag string, toolParser string) (string, testcontainers.Container, func(), error) {
	// Use default values if none are provided.
	if model == "" {
		model = defaultModel
	}
	if tag == "" {
		tag = defaultTag
	}

	// Define a no-op cleanup function to start. This ensures we can always
	// return a valid, non-nil function.
	cleanup := func() {}
	cmd := []string{
		"--model", model,
	}
	if toolParser != "" && toolParser != "none" {
		// deepseek_v3,granite-20b-fc,granite,hermes,internlm,jamba,llama4_pythonic,llama4_json,llama3_json,mistral,phi4_mini_json,pythonic
		cmd = append(cmd, "--enable-auto-tool-choice", "--tool-call-parser", toolParser)
	}
	req := testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: "openeuler/vllm-cpu:" + tag,
			Env: map[string]string{
				"MODEL":         model,
				"MAX_MODEL_LEN": defaultMaxModelLen,
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

	// Perform a secondary, more reliable readiness check by polling the /v1/models endpoint.
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
		// Create a new request with the parent context for cancellation propagation.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create models request: %w", err)
		}

		resp, err := client.Do(req)
		if err == nil {
			// Ensure the body is always closed and drained.
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
				return nil // Success!
			}
			log.Printf("vLLM models check returned status %d (attempt %d/%d)", resp.StatusCode, i+1, readinessRetries)
		} else {
			log.Printf("vLLM models check failed (attempt %d/%d): %v", i+1, readinessRetries, err)
		}

		// Wait before retrying, but respect context cancellation.
		select {
		case <-time.After(pollInterval):
			// Continue to next iteration.
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return fmt.Errorf("timed out after %d retries", readinessRetries)
}
