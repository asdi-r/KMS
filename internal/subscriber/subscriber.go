// Package subscriber is the "Subscriber" box: consumes key events from the
// queue, optionally forwards them to a webhook, and records delivery.
// Any returned error makes the queue retry the message.
package subscriber

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"kms/internal/queue"
	"kms/internal/store"
)

type Subscriber struct {
	store      *store.Store
	webhookURL string
	http       *http.Client
}

func New(s *store.Store, webhookURL string) *Subscriber {
	return &Subscriber{store: s, webhookURL: webhookURL, http: &http.Client{Timeout: 10 * time.Second}}
}

func (s *Subscriber) Handle(ctx context.Context, m queue.Message) error {
	var ev struct {
		Key        string `json:"key"`
		CustomerID string `json:"customer_id"`
		Product    string `json:"product"`
	}
	if err := json.Unmarshal(m.Body, &ev); err != nil {
		return fmt.Errorf("bad payload: %w", err) // ends up in DLQ after retries
	}

	if s.webhookURL != "" {
		payload, _ := json.Marshal(map[string]any{"id": m.ID, "type": m.Type, "data": json.RawMessage(m.Body)})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhookURL, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := s.http.Do(req)
		if err != nil {
			return fmt.Errorf("webhook: %w", err)
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("webhook status %d", resp.StatusCode)
		}
	}

	first, err := s.store.RecordDelivery(ctx, m.ID, ev.Key, m.Attempts)
	if err != nil {
		return fmt.Errorf("record delivery: %w", err)
	}
	slog.Info("processed", "id", m.ID, "type", m.Type, "key", ev.Key,
		"customer", ev.CustomerID, "attempt", m.Attempts, "duplicate", !first)
	return nil
}
