package types

import (
	"net/http"
	"reflect"
	"testing"
)

func TestHTTPHeaderToProtoAndBack(t *testing.T) {
	original := http.Header{
		"Accept":        {"application/json", "text/plain"},
		"X-Custom-Id":   {"123"},
		"Set-Cookie":    {"a=1", "b=2"},
		"Empty-Default": nil,
	}

	protoEntries := HTTPHeaderToProto(original)
	if protoEntries == nil {
		t.Fatal("expected non-nil proto entries")
	}

	var (
		mutatedEntry *HeaderEntry
		originalVal  string
	)

	// Mutate one of the entries with values to ensure we got deep copies.
	for _, entry := range protoEntries {
		if entry.Key == "Accept" && len(entry.Values) > 0 {
			mutatedEntry = entry
			originalVal = entry.Values[0]
			entry.Values[0] = "mutated"
			break
		}
	}
	if mutatedEntry == nil {
		t.Fatal("expected to find Accept header to mutate")
	}

	if got := original.Get("Accept"); got != "application/json" {
		t.Fatalf("expected original header unaffected by proto mutation, got %q", got)
	}

	// Restore the mutated value to compare with the original.
	mutatedEntry.Values[0] = originalVal

	back := ProtoToHTTPHeader(protoEntries)

	if got := len(back); got != len(original) {
		t.Fatalf("expected %d headers, got %d", len(original), got)
	}

	for key, values := range original {
		gotValues := back[key]
		if !reflect.DeepEqual(values, gotValues) {
			t.Fatalf("header mismatch for %s: expected %v, got %v", key, values, gotValues)
		}
		if len(gotValues) > 0 && &gotValues[0] == &values[0] {
			t.Fatalf("expected copy for header %s, but values share the same backing array", key)
		}
	}
}

func TestProtoHeaderNilHandling(t *testing.T) {
	if entries := HTTPHeaderToProto(nil); entries != nil {
		t.Fatalf("expected nil entries for nil header, got %#v", entries)
	}

	header := ProtoToHTTPHeader(nil)
	if header == nil {
		t.Fatal("expected non-nil header map for nil input")
	}
	if len(header) != 0 {
		t.Fatalf("expected empty header, got %#v", header)
	}
}
