package main

import (
	"path/filepath"
	"testing"
)

// TestLogger_RingBuffer RequestLog halka tamponunun bellekte en fazla 200
// istek tuttuğunu (RAM patlaması önlemi) doğrular.
func TestLogger_RingBuffer(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "log.jsonl")

	l := InitLogger(tmp, 0)
	if l.capacity != maxRequestLogEntries {
		t.Fatalf("varsayılan kapasite %d olmalı, alındı: %d", maxRequestLogEntries, l.capacity)
	}

	for range maxRequestLogEntries + 10 {
		l.Log(RequestLog{ID: "ring", Model: "m"})
	}

	l.mu.RLock()
	n := len(l.entries)
	l.mu.RUnlock()
	if n != maxRequestLogEntries {
		t.Fatalf("halka tamponu tam %d girdide kalmalı, alındı: %d", maxRequestLogEntries, n)
	}
}
