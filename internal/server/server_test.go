package server

import (
	"testing"
	"time"

	"github.com/zeyugao/synapse/internal/types"
)

func TestHandleForwardResponseNormalClosesChannel(t *testing.T) {
	srv := &Server{
		pendingRequests: make(map[string]chan *types.ForwardResponse),
	}

	reqID := "normal"
	ch := make(chan *types.ForwardResponse, 1)
	srv.pendingRequests[reqID] = ch

	resp := &types.ForwardResponse{
		RequestId: reqID,
		Kind:      types.ResponseKindNormal,
	}

	srv.handleForwardResponse(resp)

	select {
	case got := <-ch:
		if got != resp {
			t.Fatalf("expected response pointer, got %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for response on channel")
	}

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel to be closed after normal response")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel to close")
	}

	if _, exists := srv.pendingRequests[reqID]; exists {
		t.Fatalf("expected pending request for %s to be cleared", reqID)
	}
}

func TestHandleForwardResponseStreamKeepsChannelWhenNotDone(t *testing.T) {
	srv := &Server{
		pendingRequests: make(map[string]chan *types.ForwardResponse),
	}

	reqID := "stream-open"
	ch := make(chan *types.ForwardResponse, 1)
	srv.pendingRequests[reqID] = ch

	resp := &types.ForwardResponse{
		RequestId: reqID,
		Kind:      types.ResponseKindStream,
		Done:      false,
	}

	srv.handleForwardResponse(resp)

	select {
	case got := <-ch:
		if got != resp {
			t.Fatalf("expected streamed response pointer, got %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for streamed response")
	}

	if _, exists := srv.pendingRequests[reqID]; !exists {
		t.Fatalf("expected pending request %s to remain for ongoing stream", reqID)
	}

	select {
	case _, ok := <-ch:
		if !ok {
			t.Fatal("stream channel closed unexpectedly")
		}
	case <-time.After(25 * time.Millisecond):
		// channel remains open without new messages, which is expected
	}
}

func TestHandleForwardResponseStreamDoneClosesChannel(t *testing.T) {
	srv := &Server{
		pendingRequests: make(map[string]chan *types.ForwardResponse),
	}

	reqID := "stream-done"
	ch := make(chan *types.ForwardResponse, 1)
	srv.pendingRequests[reqID] = ch

	resp := &types.ForwardResponse{
		RequestId: reqID,
		Kind:      types.ResponseKindStream,
		Done:      true,
	}

	srv.handleForwardResponse(resp)

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream done message")
	}

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected stream channel to be closed when done")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream channel closure")
	}

	if _, exists := srv.pendingRequests[reqID]; exists {
		t.Fatalf("expected pending request %s to be removed after stream completion", reqID)
	}
}
