package main

import (
	"context"
	"time"
)

// clock is the agent layer's sense of time. The live server runs on the wall clock; replays and the simulator run
// on a scaled clock (60x: a minute of conversation in a second), so the same engine code is exercised either way.
type clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
	Speed() float64
}

type scaledClock struct {
	start  time.Time // wall time at origin
	origin time.Time // virtual time at start
	speed  float64
}

var wallClock clock = scaledClock{speed: 1}

func newScaledClock(origin time.Time, speed float64) scaledClock {
	if speed <= 0 {
		speed = 1
	}
	return scaledClock{start: time.Now(), origin: origin, speed: speed}
}

func (c scaledClock) Now() time.Time {
	if c.origin.IsZero() {
		return time.Now()
	}
	return c.origin.Add(time.Duration(float64(time.Since(c.start)) * c.speed))
}

func (c scaledClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(time.Duration(float64(d) / c.speed))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c scaledClock) Speed() float64 { return c.speed }
