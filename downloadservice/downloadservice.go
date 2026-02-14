package downloadservice

import (
	"context"
	"encoding/json"
	"log"
	"time"

	libbus "github.com/contenox/vibe/libbus"
	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/runtimetypes"
)

var (
	_ Service = &service{}
)

type Service interface {
	CurrentDownloadQueueState(ctx context.Context) ([]Job, error)
	CancelDownloads(ctx context.Context, url string) error
	RemoveDownloadFromQueue(ctx context.Context, modelName string) error
	DownloadInProgress(ctx context.Context, statusCh chan<- *runtimetypes.Status) error
}

type service struct {
	dbInstance libdb.DBManager
	psInstance libbus.Messenger
}

func New(dbInstance libdb.DBManager, psInstance libbus.Messenger) Service {
	return &service{
		dbInstance: dbInstance,
		psInstance: psInstance,
	}
}

type Job struct {
	ID           string                 `json:"id" example:"1234567890"`
	TaskType     string                 `json:"taskType" example:"model_download"`
	ModelJob     runtimetypes.QueueItem `json:"modelJob"`
	ScheduledFor int64                  `json:"scheduledFor" example:"1630483200"`
	ValidUntil   int64                  `json:"validUntil" example:"1630483200"`
	CreatedAt    time.Time              `json:"createdAt" example:"2021-09-01T00:00:00Z"`
}

func (s *service) CurrentDownloadQueueState(ctx context.Context) ([]Job, error) {
	tx := s.dbInstance.WithoutTransaction()
	queue, err := runtimetypes.New(tx).GetJobsForType(ctx, "model_download")
	if err != nil {
		return nil, err
	}
	var convQueue []Job
	var item runtimetypes.QueueItem
	for _, queue := range queue {

		err := json.Unmarshal(queue.Payload, &item)
		if err != nil {
			return nil, err
		}
		convQueue = append(convQueue, Job{
			ID:           queue.ID,
			TaskType:     queue.TaskType,
			ModelJob:     item,
			ScheduledFor: queue.ScheduledFor,
			ValidUntil:   queue.ValidUntil,
			CreatedAt:    queue.CreatedAt,
		})
	}

	return convQueue, nil
}

func (s *service) CancelDownloads(ctx context.Context, url string) error {
	queueItem := runtimetypes.Job{
		ID: url,
	}
	b, err := json.Marshal(&queueItem)
	if err != nil {
		return err
	}
	return s.psInstance.Publish(ctx, "queue_cancel", b)
}

func (s *service) RemoveDownloadFromQueue(ctx context.Context, modelName string) error {
	tx, comm, rTx, err := s.dbInstance.WithTransaction(ctx)
	defer func() {
		if err := rTx(); err != nil {
			log.Println("failed to rollback transaction", err)
		}
	}()
	if err != nil {
		return err
	}
	jobs, err := runtimetypes.New(tx).PopJobsForType(ctx, "model_download")
	if err != nil {
		return err
	}
	found := false
	var filteresJobs []*runtimetypes.Job
	for _, job := range jobs {
		var item runtimetypes.QueueItem
		err = json.Unmarshal(job.Payload, &item)
		if err != nil {
			return err
		}
		if item.Model != modelName {
			filteresJobs = append(filteresJobs, job)
		}
		if item.Model == modelName {
			found = true
		}
	}
	for _, job := range filteresJobs {
		err := runtimetypes.New(tx).AppendJob(ctx, *job)
		if err != nil {
			return err
		}
	}
	if found {
		return comm(ctx)
	}
	return nil
}

func (s *service) DownloadInProgress(ctx context.Context, statusCh chan<- *runtimetypes.Status) error {
	ch := make(chan []byte, 16)
	sub, err := s.psInstance.Stream(ctx, "model_download", ch)
	if err != nil {
		return err
	}
	go func() {
		defer sub.Unsubscribe()
		for {
			select {
			case data, ok := <-ch:
				if !ok {
					return
				}
				var st runtimetypes.Status
				if err := json.Unmarshal(data, &st); err != nil {
					log.Printf("failed to unmarshal status: %v", err)
					continue
				}
				if len(st.BaseURL) == 0 {
					log.Printf("BUG: len(st.BaseURL) == 0")
					continue
				}
				select {
				case statusCh <- &st:
				default:
					// If the channel is full, skip sending.
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	<-ctx.Done()

	if err := sub.Unsubscribe(); err != nil {
		return err
	}

	return nil
}
