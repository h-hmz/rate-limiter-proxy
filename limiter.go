package main

import (
	"fmt"
	"time"

	ratelimiter "github.com/h-hmz/rate-limiter"
	"github.com/redis/go-redis/v9"

	rlstorage "github.com/h-hmz/rate-limiter/storage"
	"github.com/h-hmz/rate-limiter/tokenbucket"

	"github.com/h-hmz/rate-limiter/fixedwindow"
)

const memoryGCInterval = time.Minute

func buildLimiter(cfg Config) (ratelimiter.Limiter, error) {
	clock := &ratelimiter.WallClock{}

	var limiter ratelimiter.Limiter

	switch cfg.Algorithm {
	case algFixedWindow:

		store, err := buildStore[fixedwindow.State](cfg, clock)
		if err != nil {
			return nil, err
		}
		limiter = fixedwindow.New(int64(cfg.Rate), cfg.Window, store, clock)

	case algTokenBucket:
		store, err := buildStore[tokenbucket.State](cfg, clock)
		if err != nil {
			return nil, err
		}
		limiter = tokenbucket.New(cfg.Rate, cfg.Burst, store, clock)

	default:
		return nil, fmt.Errorf("unknown algorithm %q", cfg.Algorithm)
	}

	return limiter, nil
}

func buildStore[T any](cfg Config, clock ratelimiter.Clock) (rlstorage.Store[T], error) {
	switch cfg.Store {
	case storeRedis:
		client := redis.NewClient(&redis.Options{
			Addr:     cfg.RedisAddr,
			Password: cfg.RedisPassword,
		})
		return rlstorage.NewRedisStore[T](client), nil

	case storeMemory:
		store := rlstorage.NewInMemoryStore[T](clock)
		store.StartGC(memoryGCInterval)
		return store, nil

	default:
		return nil, fmt.Errorf("unknown store %q", cfg.Store)
	}
}
