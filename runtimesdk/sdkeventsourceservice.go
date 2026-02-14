package runtimesdk

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/eventsourceservice"
	"github.com/contenox/vibe/eventstore"
)

// HTTPEvenSourceService implements the eventsourceservice.Service interface
// using HTTP calls to the event source API.
type HTTPEvenSourceService struct {
	client  *http.Client
	baseURL string
	token   string
}

// NewHTTPEvenSourceService creates a new HTTP client that implements eventsourceservice.Service.
func NewHTTPEvenSourceService(baseURL, token string, client *http.Client) eventsourceservice.Service {
	if client == nil {
		client = http.DefaultClient
	}
	baseURL = strings.TrimSuffix(baseURL, "/")
	return &HTTPEvenSourceService{
		client:  client,
		baseURL: baseURL,
		token:   token,
	}
}

// GetRawEvent implements eventsourceservice.Service.GetRawEvent.
func (s *HTTPEvenSourceService) GetRawEvent(
	ctx context.Context,
	from, to time.Time,
	nid int64,
) (*eventstore.RawEvent, error) {
	u, err := url.Parse(fmt.Sprintf("%s/raw-events/%d", s.baseURL, nid))
	if err != nil {
		return nil, err
	}

	q := u.Query()
	q.Set("from", from.Format(time.RFC3339))
	q.Set("to", to.Format(time.RFC3339))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}

	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, apiframework.HandleAPIError(resp)
	}

	var rawEvent eventstore.RawEvent
	if err := json.NewDecoder(resp.Body).Decode(&rawEvent); err != nil {
		return nil, fmt.Errorf("failed to decode raw event: %w", err)
	}

	return &rawEvent, nil
}

// ListRawEvents implements eventsourceservice.Service.ListRawEvents.
func (s *HTTPEvenSourceService) ListRawEvents(
	ctx context.Context,
	from, to time.Time,
	limit int,
) ([]*eventstore.RawEvent, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("limit must be positive")
	}

	u, err := url.Parse(s.baseURL + "/raw-events")
	if err != nil {
		return nil, err
	}

	q := u.Query()
	q.Set("from", from.Format(time.RFC3339))
	q.Set("to", to.Format(time.RFC3339))
	q.Set("limit", fmt.Sprintf("%d", limit))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}

	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, apiframework.HandleAPIError(resp)
	}

	var rawEvents []*eventstore.RawEvent
	if err := json.NewDecoder(resp.Body).Decode(&rawEvents); err != nil {
		return nil, fmt.Errorf("failed to decode raw events: %w", err)
	}

	return rawEvents, nil
}

// AppendEvent implements eventsourceservice.Service.AppendEvent.
func (s *HTTPEvenSourceService) AppendEvent(ctx context.Context, event *eventstore.Event) error {
	url := s.baseURL + "/events"

	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}
	req.Body = io.NopCloser(strings.NewReader(string(body)))

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return apiframework.HandleAPIError(resp)
	}

	// Update event with any server-assigned fields (ID, CreatedAt, etc.)
	return json.NewDecoder(resp.Body).Decode(event)
}

// GetEventsByAggregate implements eventsourceservice.Service.GetEventsByAggregate.
func (s *HTTPEvenSourceService) GetEventsByAggregate(
	ctx context.Context,
	eventType string,
	from, to time.Time,
	aggregateType, aggregateID string,
	limit int,
) ([]eventstore.Event, error) {
	if eventType == "" {
		return nil, fmt.Errorf("eventType is required")
	}
	if aggregateType == "" {
		return nil, fmt.Errorf("aggregateType is required")
	}
	if aggregateID == "" {
		return nil, fmt.Errorf("aggregateID is required")
	}
	if limit <= 0 {
		return nil, fmt.Errorf("limit must be positive")
	}

	u, err := url.Parse(s.baseURL + "/events/aggregate")
	if err != nil {
		return nil, err
	}

	q := u.Query()
	q.Set("event_type", eventType)
	q.Set("aggregate_type", aggregateType)
	q.Set("aggregate_id", aggregateID)
	q.Set("from", from.Format(time.RFC3339))
	q.Set("to", to.Format(time.RFC3339))
	q.Set("limit", fmt.Sprintf("%d", limit))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}

	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, apiframework.HandleAPIError(resp)
	}

	var events []eventstore.Event
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return nil, fmt.Errorf("failed to decode events: %w", err)
	}

	return events, nil
}

// GetEventsByType implements eventsourceservice.Service.GetEventsByType.
func (s *HTTPEvenSourceService) GetEventsByType(
	ctx context.Context,
	eventType string,
	from, to time.Time,
	limit int,
) ([]eventstore.Event, error) {
	if eventType == "" {
		return nil, fmt.Errorf("eventType is required")
	}
	if limit <= 0 {
		return nil, fmt.Errorf("limit must be positive")
	}

	u, err := url.Parse(s.baseURL + "/events/type")
	if err != nil {
		return nil, err
	}

	q := u.Query()
	q.Set("event_type", eventType)
	q.Set("from", from.Format(time.RFC3339))
	q.Set("to", to.Format(time.RFC3339))
	q.Set("limit", fmt.Sprintf("%d", limit))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}

	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, apiframework.HandleAPIError(resp)
	}

	var events []eventstore.Event
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return nil, fmt.Errorf("failed to decode events: %w", err)
	}

	return events, nil
}

// GetEventsBySource implements eventsourceservice.Service.GetEventsBySource.
func (s *HTTPEvenSourceService) GetEventsBySource(
	ctx context.Context,
	eventType string,
	from, to time.Time,
	eventSource string,
	limit int,
) ([]eventstore.Event, error) {
	if eventType == "" {
		return nil, fmt.Errorf("eventType is required")
	}
	if eventSource == "" {
		return nil, fmt.Errorf("eventSource is required")
	}
	if limit <= 0 {
		return nil, fmt.Errorf("limit must be positive")
	}

	u, err := url.Parse(s.baseURL + "/events/source")
	if err != nil {
		return nil, err
	}

	q := u.Query()
	q.Set("event_type", eventType)
	q.Set("event_source", eventSource)
	q.Set("from", from.Format(time.RFC3339))
	q.Set("to", to.Format(time.RFC3339))
	q.Set("limit", fmt.Sprintf("%d", limit))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}

	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, apiframework.HandleAPIError(resp)
	}

	var events []eventstore.Event
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return nil, fmt.Errorf("failed to decode events: %w", err)
	}

	return events, nil
}

// GetEventTypesInRange implements eventsourceservice.Service.GetEventTypesInRange.
func (s *HTTPEvenSourceService) GetEventTypesInRange(
	ctx context.Context,
	from, to time.Time,
	limit int,
) ([]string, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("limit must be positive")
	}

	u, err := url.Parse(s.baseURL + "/events/types")
	if err != nil {
		return nil, err
	}

	q := u.Query()
	q.Set("from", from.Format(time.RFC3339))
	q.Set("to", to.Format(time.RFC3339))
	q.Set("limit", fmt.Sprintf("%d", limit))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}

	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, apiframework.HandleAPIError(resp)
	}

	var eventTypes []string
	if err := json.NewDecoder(resp.Body).Decode(&eventTypes); err != nil {
		return nil, fmt.Errorf("failed to decode event types: %w", err)
	}

	return eventTypes, nil
}

// DeleteEventsByTypeInRange implements eventsourceservice.Service.DeleteEventsByTypeInRange.
func (s *HTTPEvenSourceService) DeleteEventsByTypeInRange(
	ctx context.Context,
	eventType string,
	from, to time.Time,
) error {
	if eventType == "" {
		return fmt.Errorf("eventType is required")
	}

	u, err := url.Parse(s.baseURL + "/events/type")
	if err != nil {
		return err
	}

	q := u.Query()
	q.Set("event_type", eventType)
	q.Set("from", from.Format(time.RFC3339))
	q.Set("to", to.Format(time.RFC3339))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "DELETE", u.String(), nil)
	if err != nil {
		return err
	}

	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apiframework.HandleAPIError(resp)
	}

	return nil
}

func (s *HTTPEvenSourceService) SubscribeToEvents(ctx context.Context, eventType string, ch chan<- []byte) (eventsourceservice.Subscription, error) {
	url := fmt.Sprintf("%s/events/stream/%s", s.baseURL, eventType)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "text/event-stream")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, apiframework.HandleAPIError(resp)
	}

	// Create a subscription that will handle the SSE stream
	sub := &httpEventSubscription{
		resp:    resp,
		eventCh: ch,
		done:    make(chan struct{}),
	}

	// Start reading the SSE stream
	go sub.readStream()

	return sub, nil
}

// httpEventSubscription implements the eventsourceservice.Subscription interface
type httpEventSubscription struct {
	resp    *http.Response
	eventCh chan<- []byte
	done    chan struct{}
}

// readStream reads the Server-Sent Events stream and sends events to the channel
func (s *httpEventSubscription) readStream() {
	defer s.resp.Body.Close()
	defer close(s.done)

	scanner := bufio.NewScanner(s.resp.Body)
	for scanner.Scan() {
		select {
		case <-s.done:
			return // Unsubscribe was called
		default:
			line := scanner.Text()
			if line == "" {
				continue
			}

			// SSE format: "data: {json}\n\n"
			if strings.HasPrefix(line, "data: ") {
				// Extract the JSON data
				data := strings.TrimPrefix(line, "data: ")
				if data == "[DONE]" {
					return // Stream ended
				}

				// Send the event data to the channel
				select {
				case s.eventCh <- []byte(data):
				case <-s.done:
					return
				}
			}
		}
	}
}

// Unsubscribe implements eventsourceservice.Subscription.Unsubscribe.
// It closes the connection and stops the stream.
func (s *httpEventSubscription) Unsubscribe() error {
	close(s.done)
	return s.resp.Body.Close()
}

// AppendRawEvent implements eventsourceservice.Service.AppendRawEvent.
func (s *HTTPEvenSourceService) AppendRawEvent(ctx context.Context, event *eventstore.RawEvent) error {
	url := s.baseURL + "/raw-events"

	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal raw event: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return apiframework.HandleAPIError(resp)
	}

	// Update raw event with any server-assigned fields (ID, NID, etc.)
	return json.NewDecoder(resp.Body).Decode(event)
}
