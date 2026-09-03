package telemetry

import (
	"context"
	"sync"
	"time"
)

// startTimeKey stores the request start time in context for latency metrics.
type startTimeKey struct{}

// ContextWithStartTime returns a context carrying the request start time.
func ContextWithStartTime(ctx context.Context) context.Context {
	return context.WithValue(ctx, startTimeKey{}, time.Now().UnixNano())
}

// StartTimeFromContext extracts the start time (0 when absent).
func StartTimeFromContext(ctx context.Context) int64 {
	if v, ok := ctx.Value(startTimeKey{}).(int64); ok {
		return v
	}
	return 0
}

// sync.MapStringBool is a tiny typed wrapper around sync.Map for the health
// gauge state, so hot paths avoid untyped assertions at call sites.
type MapStringBool struct{ m sync.Map }

func (s *MapStringBool) Store(k string, v bool) { s.m.Store(k, v) }

func (s *MapStringBool) Range(f func(k string, v bool) bool) {
	s.m.Range(func(key, value any) bool {
		return f(key.(string), value.(bool))
	})
}
