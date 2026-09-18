package handler

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestShouldRecordDownloadNarrowsArtifacts(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		status      int
		contentType string
		disposition string
		rangeHeader string
		want        bool
	}{
		{name: "zip", method: http.MethodGet, status: http.StatusOK, contentType: "application/zip", want: true},
		{name: "dmg", method: http.MethodGet, status: http.StatusOK, contentType: "application/x-apple-diskimage", want: true},
		{name: "explicit attachment", method: http.MethodGet, status: http.StatusOK, contentType: "text/plain", disposition: "attachment; filename=notes.txt", want: true},
		{name: "css asset", method: http.MethodGet, status: http.StatusOK, contentType: "text/css", want: false},
		{name: "javascript asset", method: http.MethodGet, status: http.StatusOK, contentType: "application/javascript", want: false},
		{name: "image asset", method: http.MethodGet, status: http.StatusOK, contentType: "image/png", want: false},
		{name: "legacy font asset", method: http.MethodGet, status: http.StatusOK, contentType: "application/vnd.ms-fontobject", want: false},
		{name: "wasm asset", method: http.MethodGet, status: http.StatusOK, contentType: "application/wasm", want: false},
		{name: "range request", method: http.MethodGet, status: http.StatusPartialContent, contentType: "application/zip", rangeHeader: "bytes=0-9", want: false},
		{name: "head", method: http.MethodHead, status: http.StatusOK, contentType: "application/zip", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(test.method, "https://example.test/file", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Range", test.rangeHeader)
			header := make(http.Header)
			header.Set("Content-Type", test.contentType)
			header.Set("Content-Disposition", test.disposition)
			if got := shouldRecordDownload(request, test.status, header); got != test.want {
				t.Fatalf("shouldRecordDownload = %t, want %t", got, test.want)
			}
		})
	}
}

func TestFileDownloadRecorderIsBoundedAndNonblocking(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	processed := make(chan string, 2)
	var once sync.Once
	recorder := newFileDownloadRecorderWith(1, 1, func(_ context.Context, event fileDownloadEvent) error {
		once.Do(func() { close(started) })
		<-release
		processed <- event.path
		return nil
	})

	if !recorder.Enqueue(fileDownloadEvent{path: "one.zip"}) {
		t.Fatal("first event was not admitted")
	}
	<-started
	if !recorder.Enqueue(fileDownloadEvent{path: "two.dmg"}) {
		t.Fatal("queued event was not admitted")
	}
	start := time.Now()
	if recorder.Enqueue(fileDownloadEvent{path: "three.zip"}) {
		t.Fatal("event exceeded queue bound")
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("saturated enqueue blocked for %v", elapsed)
	}
	close(release)

	for range 2 {
		select {
		case <-processed:
		case <-time.After(time.Second):
			t.Fatal("admitted event was not processed")
		}
	}
}
