package gdrive

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTransportConcurrencyCap(t *testing.T) {
	var inFlight atomic.Int32
	var maxObserved atomic.Int32
	releaseCh := make(chan struct{})

	tr := NewTransport(func(ctx context.Context, _ *HTTPRequest) (*HTTPResponse, error) {
		cur := inFlight.Add(1)
		for {
			prev := maxObserved.Load()
			if cur <= prev || maxObserved.CompareAndSwap(prev, cur) {
				break
			}
		}
		select {
		case <-releaseCh:
		case <-ctx.Done():
			inFlight.Add(-1)
			return nil, ctx.Err()
		}
		inFlight.Add(-1)
		return &HTTPResponse{StatusCode: http.StatusOK}, nil
	}, 2)

	client := &http.Client{Transport: tr}
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, "https://example.test/files", nil)
			resp, err := client.Do(req)
			if err != nil {
				t.Errorf("Do failed: %v", err)
				return
			}
			_ = resp.Body.Close()
		}()
	}

	time.Sleep(25 * time.Millisecond)
	if got := maxObserved.Load(); got > 2 {
		t.Fatalf("maxObserved = %d, want <= 2", got)
	}
	close(releaseCh)
	wg.Wait()
	if got := maxObserved.Load(); got != 2 {
		t.Fatalf("maxObserved = %d, want 2", got)
	}
}

func TestTransportWrapsNetworkErrorAsOffline(t *testing.T) {
	tr := NewTransport(func(_ context.Context, _ *HTTPRequest) (*HTTPResponse, error) {
		return nil, errors.New("dns failure")
	}, 1)
	client := &http.Client{Transport: tr}
	req, _ := http.NewRequest(http.MethodGet, "https://example.test/files", nil)
	_, err := client.Do(req)
	if !errors.Is(err, ErrOffline) {
		t.Fatalf("err = %v, want ErrOffline", err)
	}
}
