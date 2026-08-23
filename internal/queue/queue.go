// Package queue is the "SQS" box. The self-hosted implementation uses Redis
// Streams with a consumer group, which gives SQS-like semantics: at-least-once
// delivery, per-message ack, redelivery of unacked messages after a visibility
// timeout (retry), and a dead-letter queue after max retries.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type Message struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Body     json.RawMessage `json:"body"`
	Attempts int             `json:"attempts"`
}

type Queue struct {
	rdb        *redis.Client
	stream     string
	dlq        string
	group      string
	maxRetries int
	retryDelay time.Duration
}

func New(rdb *redis.Client, stream, dlq, group string, maxRetries int, retryDelay time.Duration) *Queue {
	return &Queue{rdb: rdb, stream: stream, dlq: dlq, group: group, maxRetries: maxRetries, retryDelay: retryDelay}
}

// Publish is the "Publisher" box.
func (q *Queue) Publish(ctx context.Context, typ string, body any) (string, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	return q.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: q.stream,
		MaxLen: 100000, Approx: true,
		Values: map[string]any{"type": typ, "body": string(b)},
	}).Result()
}

type Handler func(ctx context.Context, m Message) error

// Consume runs until ctx is cancelled. Messages whose handler fails stay
// pending and are re-claimed after retryDelay; after maxRetries they go to the DLQ.
func (q *Queue) Consume(ctx context.Context, consumer string, h Handler) error {
	err := q.rdb.XGroupCreateMkStream(ctx, q.stream, q.group, "0").Err()
	if err != nil && !strings.HasPrefix(err.Error(), "BUSYGROUP") {
		return err
	}
	for ctx.Err() == nil {
		// 1) Retry path: reclaim messages idle (failed/stalled) longer than retryDelay.
		claimed, _, err := q.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream: q.stream, Group: q.group, Consumer: consumer,
			MinIdle: q.retryDelay, Start: "0", Count: 10,
		}).Result()
		if err != nil && ctx.Err() == nil {
			slog.Warn("xautoclaim", "err", err)
		}
		for _, m := range claimed {
			q.handle(ctx, m, h)
		}
		// 2) New messages.
		res, err := q.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: q.group, Consumer: consumer,
			Streams: []string{q.stream, ">"}, Count: 10, Block: 2 * time.Second,
		}).Result()
		if err != nil {
			if errors.Is(err, redis.Nil) || ctx.Err() != nil {
				continue
			}
			slog.Warn("xreadgroup", "err", err)
			time.Sleep(time.Second)
			continue
		}
		for _, s := range res {
			for _, m := range s.Messages {
				q.handle(ctx, m, h)
			}
		}
	}
	return ctx.Err()
}

func (q *Queue) attempts(ctx context.Context, id string) int {
	ext, err := q.rdb.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: q.stream, Group: q.group, Start: id, End: id, Count: 1,
	}).Result()
	if err != nil || len(ext) != 1 {
		return 1
	}
	return int(ext[0].RetryCount)
}

func (q *Queue) handle(ctx context.Context, xm redis.XMessage, h Handler) {
	typ, _ := xm.Values["type"].(string)
	body, _ := xm.Values["body"].(string)
	msg := Message{ID: xm.ID, Type: typ, Body: json.RawMessage(body), Attempts: q.attempts(ctx, xm.ID)}

	if err := h(ctx, msg); err != nil {
		slog.Error("handler failed", "id", msg.ID, "attempt", msg.Attempts, "err", err)
		if msg.Attempts >= q.maxRetries {
			slog.Error("max retries reached, moving to DLQ", "id", msg.ID)
			q.rdb.XAdd(ctx, &redis.XAddArgs{Stream: q.dlq, Values: map[string]any{
				"type": typ, "body": body, "origin_id": msg.ID, "error": err.Error(),
			}})
			q.rdb.XAck(ctx, q.stream, q.group, xm.ID)
		}
		return // stays pending -> redelivered by XAUTOCLAIM after retryDelay
	}
	q.rdb.XAck(ctx, q.stream, q.group, xm.ID)
}
