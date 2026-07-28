package vllm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/contenox/contenox/internal/libtracker"
	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/models/modelrepo/codec/chatcompletions"
)

type VLLMStreamClient struct {
	vLLMClient
}

func NewVLLMStreamClient(ctx context.Context, baseURL, modelName string, contextLength, maxOutputTokens int, httpClient *http.Client, apiKey string, canThink bool, tracker libtracker.ActivityTracker) (modelrepo.LLMStreamClient, error) {
	client := &VLLMStreamClient{
		vLLMClient: vLLMClient{
			baseURL:         baseURL,
			httpClient:      httpClient,
			modelName:       modelName,
			maxOutputTokens: maxOutputTokens,
			canThink:        canThink,
			apiKey:          apiKey,
			tracker:         tracker,
		},
	}

	client.maxTokens = min(contextLength, 2048)
	return client, nil
}

// streamErrorChunk detects vLLM's in-stream error frames, which arrive as a
// chunk with a top-level "error" string instead of choices.
type streamErrorChunk struct {
	Error *string `json:"error"`
}

// Stream emits raw deltas per the modelrepo.StreamParcel contract via the
// shared chat/completions codec: content / thinking / tool-call fragments as
// they arrive, then one typed terminal parcel. Assembly belongs to the
// engine-side modelrepo.StreamAssembler, never to this client.
func (c *VLLMStreamClient) Stream(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (<-chan *modelrepo.StreamParcel, error) {
	reportErr, reportChange, end := c.tracker.Start(ctx, "stream", "vllm", "model", c.modelName)
	// end() is not deferred here; the stream is asynchronous, so it runs from
	// the goroutine below instead.

	request, nameMap := buildChatRequest(c.modelName, messages, args, c.canThink)
	c.clampChatRequest(&request)
	request.Stream = true
	request.StreamOptions = &streamOptions{IncludeUsage: true}

	url := c.baseURL + "/v1/chat/completions"
	reqBody, err := json.Marshal(request)
	if err != nil {
		end()
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		end()
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	streamCh := make(chan *modelrepo.StreamParcel)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		err = fmt.Errorf("HTTP request failed for model %s: %w", c.modelName, err)
		reportErr(err)
		end()
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		err = fmt.Errorf("vLLM API returned non-200 status: %d - %s for model %s",
			resp.StatusCode, string(body), c.modelName)
		err = modelrepo.ClassifyProviderError(err, resp.StatusCode, "", string(body))
		reportErr(err)
		end()
		return nil, err
	}

	go func() {
		defer close(streamCh)
		defer resp.Body.Close()
		defer end()

		send := func(p *modelrepo.StreamParcel) bool {
			select {
			case streamCh <- p:
				return true
			case <-ctx.Done():
				return false
			}
		}

		// Tool names are sanitized on the way out; the decoder translates the
		// sanitized names back to the caller's originals.
		dec := chatcompletions.NewStreamDecoder(nameMap)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		var chunkCount int

		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			jsonData := strings.TrimPrefix(line, "data: ")
			if jsonData == "[DONE]" {
				continue
			}

			// Handle error chunks before delta decoding.
			var errChunk streamErrorChunk
			if json.Unmarshal([]byte(jsonData), &errChunk) == nil && errChunk.Error != nil {
				err := fmt.Errorf("vLLM stream error: %s", *errChunk.Error)
				reportErr(err)
				send(&modelrepo.StreamParcel{Error: err})
				return
			}

			parcels, derr := dec.DecodeLine([]byte(jsonData))
			if derr != nil {
				derr = fmt.Errorf("vLLM stream decode failed for model %s: %w", c.modelName, derr)
				reportErr(derr)
				send(&modelrepo.StreamParcel{Error: derr})
				return
			}
			for _, p := range parcels {
				chunkCount++
				if !send(p) {
					return
				}
			}
		}

		if err := scanner.Err(); err != nil && err != io.EOF {
			err := fmt.Errorf("stream scanning error: %w", err)
			reportErr(err)
			send(&modelrepo.StreamParcel{Error: err})
			return
		}

		if !send(dec.Finish()) {
			return
		}
		reportChange("stream_completed", map[string]any{"chunk_count": chunkCount})
	}()

	return streamCh, nil
}

var _ modelrepo.LLMStreamClient = (*VLLMStreamClient)(nil)
