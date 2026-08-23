// Package cache is the "redis" box used by Key Validation.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

type Cache struct {
	rdb *redis.Client
	ttl time.Duration
}

func New(rdb *redis.Client, ttl time.Duration) *Cache { return &Cache{rdb: rdb, ttl: ttl} }

func (c *Cache) Get(ctx context.Context, key string, v any) (bool, error) {
	b, err := c.rdb.Get(ctx, "key:"+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

func (c *Cache) Set(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, "key:"+key, b, c.ttl).Err()
}

func (c *Cache) Del(ctx context.Context, key string) error {
	return c.rdb.Del(ctx, "key:"+key).Err()
}
