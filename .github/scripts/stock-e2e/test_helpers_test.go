package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

var errSentinel = errors.New("transient")

func contextWithTimeout(t *testing.T, timeout time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), timeout)
}
