package runtimestate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/runtimetypes"

	"github.com/ollama/ollama/api"
)

// dwqueue manages the persistence and retrieval of model download tasks
// using the provided database instance (`dbInstance`).
type dwqueue struct {
	dbInstance libdb.DBManager
}

// add enqueues one or more download tasks for the specified models from a given backend URL.
// It stores these tasks persistently using the underlying dbInstance.
//
// Rationale for Job ID: It uses the backend's base URL (u.String()) as the Job ID
// in the persistent queue. Because "Sync Cycle is the Source of Truth" -> the sync
// cycle will run again and will re-detect all currently missing models,
// the queue doesn't need to persistently store a comprehensive list
// of every single model that needs downloading for a backend.
func (q dwqueue) add(ctx context.Context, u url.URL, models ...string) error {
	tx := q.dbInstance.WithoutTransaction()
	for _, model := range models {
		payload, err := json.Marshal(runtimetypes.QueueItem{URL: u.String(), Model: model})
		if err != nil {
			return err
		}
		err = runtimetypes.New(tx).AppendJob(ctx, runtimetypes.Job{
			ID:       u.String(), // Using backends url as ID to prevent multiple downloads on the same backend
			TaskType: "model_download",
			Payload:  payload,
		})
		if err != nil {
			_ = err
			// log.Println("Expected error adding job to queue:", err)
		}
	}

	return nil
}

// pop retrieves and removes the next pending 'model_download' task from the persistent queue.
// It returns the details of the task (URL and Model name) within a QueueItem.
// If no 'model_download' tasks are currently pending in the queue, it returns libdb.ErrNotFound.
func (q dwqueue) pop(ctx context.Context) (*runtimetypes.QueueItem, error) {
	tx := q.dbInstance.WithoutTransaction()

	job, err := runtimetypes.New(tx).PopJobForType(ctx, "model_download")
	if err != nil {
		return nil, err
	}
	var item runtimetypes.QueueItem
	// Use &item so json.Unmarshal writes into our allocated struct.
	err = json.Unmarshal(job.Payload, &item)
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// downloadModel executes the actual model download process for a given task item.
// It uses the Ollama API client to pull the specified model from the backend URL defined in the item.
// The progress function is called periodically during the download with status updates.
func (q dwqueue) downloadModel(ctx context.Context, item runtimetypes.QueueItem, progress func(status runtimetypes.Status) error) error {
	u, err := url.Parse(item.URL)
	if err != nil {
		return err
	}
	client := api.NewClient(u, http.DefaultClient)

	err = client.Pull(ctx, &api.PullRequest{
		Model: item.Model,
	}, func(pr api.ProgressResponse) error {
		return progress(runtimetypes.Status{
			Digest:    pr.Digest,
			Status:    pr.Status,
			Total:     pr.Total,
			Completed: pr.Completed,
			Model:     item.Model,
			BaseURL:   item.URL,
		})
	})
	if err != nil {
		return fmt.Errorf("failed to fetch %s: %v", item.URL, err)
	}
	return nil
}
