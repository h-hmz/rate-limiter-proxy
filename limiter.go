package main

import (
	"context"
	"fmt"
	"log"
	"runtime"
	"sync/atomic"
	"time"

	ratelimiter "github.com/h-hmz/rate-limiter"
	"github.com/redis/go-redis/v9"

	rlstorage "github.com/h-hmz/rate-limiter/storage"
	"github.com/h-hmz/rate-limiter/tokenbucket"

	"github.com/h-hmz/rate-limiter/fixedwindow"
)

const memoryGCInterval = time.Minute

// go-redis is tuned for general-purpose work, where waiting is better than
// failing. A rate limiter sits in the request path and wants the opposite: a
// slow answer is worse than no answer, because every millisecond spent asking
// permission is added to a request that has not started yet. These values trade
// the stock timeouts down accordingly.
const (
	redisDialTimeout   = time.Second
	redisDialerRetries = 1
	redisMaxRetries    = 1

	// redisIOTimeout bounds a single read or write. A co-located Redis answers
	// a rate limit transaction in single-digit milliseconds, so this leaves
	// roughly fifty times the expected p99 as headroom for a GC pause or a
	// background save.
	redisIOTimeout = 250 * time.Millisecond

	// redisMinPoolSize is a floor under the stock 10*GOMAXPROCS. Go sizes
	// GOMAXPROCS from the cgroup CPU limit, so a proxy capped at one core would
	// otherwise get a 10 connection pool while serving far more concurrent
	// requests than that.
	redisMinPoolSize = 64

	redisMinIdleConns = 8
)

// startupProbeTimeout is more generous than the runtime probe: a cold
// container's first dial can be slower than at steady-state.
const startupProbeTimeout = 3 * time.Second

// Health probing cadence. Used in probeLoop.
const (
	healthyPoll   = time.Second
	unhealthyPoll = 100 * time.Millisecond
	probeTimeout  = time.Second

	// consecutive failed probes it takes to stop trusting the store.
	unhealthyThreshold = 3
)

// pollClock is used by probeLoop needs, to drive tests without sleeping.
// The realClock implementation is used in production.
type pollClock interface {
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// storeOps are the operational hooks a backend exposes to the proxy: how to
// probe its health, and how to shut it down. A nil *storeOps means the backend
// cannot fail and holds nothing worth closing (memory).
type storeOps struct {
	probe func(ctx context.Context) error
	close func() error
}

type failOpenLimiter struct {
	inner  ratelimiter.Limiter
	isOpen atomic.Bool
}

var _ ratelimiter.Limiter = &failOpenLimiter{}

func (l *failOpenLimiter) Allow(ctx context.Context, key string) (ratelimiter.Result, error) {
	// The store is known to be unhealthy: don't even try the limiter.
	if l.isOpen.Load() {
		return ratelimiter.Result{Allowed: true}, nil
	}

	res, err := l.inner.Allow(ctx, key)
	if err != nil {
		// Fail open.
		return ratelimiter.Result{Allowed: true}, nil
	}

	return res, nil
}

// buildLimiter assembles the configured limiter. The returned close func (nil
// when the store holds no resources) is the caller's to run, and only after
// in-flight requests have drained.
func buildLimiter(ctx context.Context, cfg Config) (ratelimiter.Limiter, func() error, error) {
	clock := &ratelimiter.WallClock{}

	var limiter ratelimiter.Limiter
	var ops *storeOps
	var err error

	switch cfg.Algorithm {
	case algFixedWindow:
		var store rlstorage.Store[fixedwindow.State]

		store, ops, err = buildStore[fixedwindow.State](cfg, clock)
		if err != nil {
			return nil, nil, err
		}
		limiter = fixedwindow.New(int64(cfg.Rate), cfg.Window, store, clock)

	case algTokenBucket:
		var store rlstorage.Store[tokenbucket.State]

		store, ops, err = buildStore[tokenbucket.State](cfg, clock)
		if err != nil {
			return nil, nil, err
		}
		limiter = tokenbucket.New(cfg.Rate, cfg.Burst, store, clock)

	default:
		return nil, nil, fmt.Errorf("unknown algorithm %q", cfg.Algorithm)
	}

	var closeStore func() error
	if ops != nil {
		closeStore = ops.close

		// At boot, an outage and a typo'd RL_REDIS_ADDR are indistinguishable,
		// and the typo would mean running with no rate limiting, silently and
		// forever. Refuse to start instead: a crash loop is loud, and if it
		// really is an outage, it restarts until the store comes back.
		// This applies to fail-closed too, where a bad address would mean
		// rejecting every request instead.
		pctx, cancel := context.WithTimeout(ctx, startupProbeTimeout)
		probeErr := ops.probe(pctx)
		cancel()
		if probeErr != nil {
			_ = ops.close()
			return nil, nil, fmt.Errorf("store unreachable at startup: %w", probeErr)
		}
	}

	if cfg.FailOpen && ops != nil {
		lim := &failOpenLimiter{inner: limiter}
		go probeLoop(ctx, lim, ops.probe, realClock{})
		return lim, closeStore, nil
	}

	return limiter, closeStore, nil
}

// probeLoop watches the store's health and flips lim between enforcing and
// failing open. It exits when ctx is cancelled; closing the store afterwards
// is the caller's job.
func probeLoop(ctx context.Context, lim *failOpenLimiter, probe func(ctx context.Context) error, clk pollClock) {
	fails := 0
	down := false

	for {
		// pctx bounds this one probe. It ends when probeTimeout elapses or
		// when ctx (shutdown) is cancelled, whichever comes first.
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		err := probe(pctx)
		cancel()

		// A non-nil err is ambiguous on its own, because pctx can die two
		// ways that mean opposite things:
		//
		//   - probeTimeout fired -> err is context.DeadlineExceeded.
		//     The store really is too slow; that must count as a failure.
		//   - shutdown cancelled ctx mid-probe -> err is context.Canceled.
		//     That says nothing about the store.
		//
		// Asking the parent separates them: ctx.Err() is non-nil only for
		// shutdown. Without this guard, a shutdown landing mid-probe would
		// be scored as a store failure below
		if ctx.Err() != nil {
			return
		}

		poll := unhealthyPoll
		if err == nil {
			if down {
				log.Printf("store is reachable again, rate limiting restored")
			}
			down = false
			fails = 0
			lim.isOpen.Store(false)
			poll = healthyPoll
		} else {
			fails++
			if fails >= unhealthyThreshold {
				if !down {
					log.Printf("store is unreachable, failing open: %v", err)
				}
				down = true
				lim.isOpen.Store(true)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-clk.After(poll):
		}
	}
}

func buildStore[T any](cfg Config, clock ratelimiter.Clock) (rlstorage.Store[T], *storeOps, error) {
	switch cfg.Store {
	case storeRedis:
		client := buildRedisClient(cfg)
		ops := &storeOps{
			probe: func(ctx context.Context) error { return client.Ping(ctx).Err() },
			close: client.Close,
		}
		return rlstorage.NewRedisStore[T](client), ops, nil

	case storeMemory:
		store := rlstorage.NewInMemoryStore[T](clock)
		store.StartGC(memoryGCInterval)
		return store, nil, nil

	default:
		return nil, nil, fmt.Errorf("unknown store %q", cfg.Store)
	}
}

func buildRedisClient(cfg Config) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,

		DialTimeout:   redisDialTimeout,
		DialerRetries: redisDialerRetries,
		MaxRetries:    redisMaxRetries,

		ReadTimeout:  redisIOTimeout,
		WriteTimeout: redisIOTimeout,

		PoolSize:     max(10*runtime.GOMAXPROCS(0), redisMinPoolSize),
		MinIdleConns: redisMinIdleConns,
	})

}
