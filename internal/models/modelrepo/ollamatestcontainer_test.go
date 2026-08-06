package modelrepo

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func quiet() func() {
	null, _ := os.Open(os.DevNull)
	sout := os.Stdout
	serr := os.Stderr
	os.Stdout = null
	os.Stderr = null
	log.SetOutput(null)
	return func() {
		defer null.Close()
		os.Stdout = sout
		os.Stderr = serr
		log.SetOutput(os.Stderr)
	}
}

func SetupOllamaLocalInstance(ctx context.Context, tag string) (string, testcontainers.Container, func(), error) {
	defer quiet()()

	cleanup := func() {}
	exposedPort := "11434/tcp"
	if tag == "" {
		tag = "latest"
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:           "ollama/ollama:" + tag,
			ExposedPorts:    []string{exposedPort},
			WaitingFor:      wait.ForHTTP("/").WithStartupTimeout(10 * time.Second),
			AlwaysPullImage: false,
		},
		Started: false,
	})
	if err != nil {
		return "", nil, cleanup, err
	}
	cleanup = func() {
		timeout := time.Second
		container.Stop(ctx, &timeout)
	}
	err = container.Start(ctx)
	if err != nil {
		return "", nil, cleanup, err
	}

	host, err := container.Host(ctx)
	if err != nil {
		return "", nil, cleanup, err
	}

	mappedPort, err := container.MappedPort(ctx, "11434")
	if err != nil {
		return "", nil, cleanup, err
	}

	uri := fmt.Sprintf("http://%s:%s", host, mappedPort.Port())
	if _, err := url.Parse(uri); err != nil {
		return "", nil, cleanup, err
	}

	const maxRetries = 5
	const retryInterval = 1 * time.Second
	var heartbeatErr error
	for attempt := range maxRetries {
		heartbeatErr = ollamaHeartbeat(ctx, uri)
		if heartbeatErr == nil {
			break
		}
		if attempt < maxRetries-1 {
			time.Sleep(retryInterval)
		}
	}
	if heartbeatErr != nil {
		return "", nil, cleanup, heartbeatErr
	}

	return uri, container, cleanup, nil
}

// ollamaHeartbeat reports whether the server answers, matching what the ollama
// CLI probes: HEAD on the root path, any non-error status.
func ollamaHeartbeat(ctx context.Context, baseURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, strings.TrimSuffix(baseURL, "/")+"/", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("ollama heartbeat returned %d", resp.StatusCode)
	}
	return nil
}
