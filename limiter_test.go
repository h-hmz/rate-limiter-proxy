package main

import (
	"context"
	"errors"
	"testing"
	"time"

	ratelimiter "github.com/h-hmz/rate-limiter"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockWait waits for the loop to park in clk.After, which proves the probe has been fully accounted for.
func blockWait(t *testing.T, clk *clockwork.FakeClock, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	require.NoError(t, clk.BlockUntilContext(ctx, n),
		"timed out waiting for the probe loop to park in clk.After")
}

type flakyProbe struct {
	failing bool
	calls   int64
}

func (p *flakyProbe) probe(ctx context.Context) error {
	p.calls++
	if p.failing {
		return errors.New("probe error")
	}
	return nil
}

func TestProbeLoopFailsOpenAfterConsecutiveFailures(t *testing.T) {
	lim := &failOpenLimiter{}
	p := &flakyProbe{failing: true}
	clk := clockwork.NewFakeClock()

	go probeLoop(t.Context(), lim, p.probe, clk)

	blockWait(t, clk, 1)
	assert.Equal(t, int64(1), p.calls)
	assert.False(t, lim.isOpen.Load(), "one failed probe must not fail open")

	clk.Advance(unhealthyPoll)
	blockWait(t, clk, 1)
	assert.Equal(t, int64(2), p.calls)
	assert.False(t, lim.isOpen.Load(), "two failed probes must not fail open")

	clk.Advance(unhealthyPoll)
	blockWait(t, clk, 1)
	assert.Equal(t, int64(3), p.calls)
	assert.True(t, lim.isOpen.Load(), "the third consecutive failed probe must fail open")
}

func TestProbeLoopFailsCloseAfterProbeSuccess(t *testing.T) {
	lim := &failOpenLimiter{}
	p := &flakyProbe{failing: true}
	clk := clockwork.NewFakeClock()

	go probeLoop(t.Context(), lim, p.probe, clk)

	for range unhealthyThreshold {
		blockWait(t, clk, 1)
		clk.Advance(unhealthyPoll)
	}
	blockWait(t, clk, 1)
	assert.True(t, lim.isOpen.Load())

	p.failing = false
	clk.Advance(unhealthyPoll)

	blockWait(t, clk, 1)
	assert.False(t, lim.isOpen.Load(), "a single healthy probe must restore rate limiting")
}

func TestProbeLoopStopsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	lim := &failOpenLimiter{}

	// The probe hangs until shutdown, mimicking a probe caught in flight.
	probe := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	done := make(chan struct{})
	go func() {
		probeLoop(ctx, lim, probe, realClock{})
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("probeLoop did not exit after cancellation")
	}
	assert.False(t, lim.isOpen.Load(), "shutdown must not be treated as a store failure")
}

type stubLimiter struct {
	err   error
	calls int64
}

func (s *stubLimiter) Allow(ctx context.Context, key string) (ratelimiter.Result, error) {
	s.calls++
	if s.err != nil {
		return ratelimiter.Result{}, s.err
	}
	return ratelimiter.Result{Allowed: true, Limit: 5, Remaining: 4}, nil
}

func TestFailOpenAllowsWhenStoreErrors(t *testing.T) {
	inner := &stubLimiter{err: errors.New("store down")}
	lim := &failOpenLimiter{inner: inner}

	res, err := lim.Allow(t.Context(), "k")
	require.NoError(t, err)
	assert.True(t, res.Allowed, "a store error must not reject the request")
}

func TestFailOpenSkipsStoreWhileDown(t *testing.T) {
	inner := &stubLimiter{}
	lim := &failOpenLimiter{inner: inner}
	lim.isOpen.Store(true)

	res, err := lim.Allow(t.Context(), "k")
	require.NoError(t, err)
	assert.True(t, res.Allowed)
	assert.Zero(t, inner.calls, "a store known to be down must not be called at all")
}
