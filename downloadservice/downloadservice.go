package downloadservice

import (
	"context"
	"time"

	"github.com/contenox/contenox/runtimetypes"
)

type Service interface {
	CurrentDownloadQueueState(ctx context.Context) ([]Job, error)
	CancelDownloads(ctx context.Context, url string) error
	RemoveDownloadFromQueue(ctx context.Context, modelName string) error
	DownloadInProgress(ctx context.Context, statusCh chan<- *runtimetypes.Status) error
}

type Job struct {
	ID           string                 `json:"id" example:"1234567890"`
	TaskType     string                 `json:"taskType" example:"download"`
	ModelJob     runtimetypes.QueueItem `json:"modelJob"`
	ScheduledFor int64                  `json:"scheduledFor" example:"1630483200"`
	ValidUntil   int64                  `json:"validUntil" example:"1630483200"`
	CreatedAt    time.Time              `json:"createdAt" example:"2021-09-01T00:00:00Z"`
}
