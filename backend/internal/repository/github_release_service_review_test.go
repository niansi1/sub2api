package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseDownloadContentRange(t *testing.T) {
	for _, value := range []string{"bytes 0-0/1", "bytes 8-15/32", "bytes 000-015/032"} {
		_, err := parseDownloadContentRange(value)
		require.NoError(t, err, value)
	}
	for _, value := range []string{
		"", "0-1/32", "items 0-1/32", "bytes 0-1/*", "bytes */32",
		"bytes -1-1/32", "bytes +0-1/32", "bytes 2-1/32", "bytes 0-32/32",
		"bytes 0-0/0", "bytes 0-1/32 junk", "bytes 0-1/32, 2-3/32",
		"bytes 0-1/9223372036854775808",
	} {
		_, err := parseDownloadContentRange(value)
		require.Error(t, err, value)
	}
}

func TestDownloadRangeProbeRequiresExactSingleByte(t *testing.T) {
	tests := []struct {
		name         string
		contentRange string
		body         string
		wantRanged   bool
	}{
		{name: "valid", contentRange: "bytes 0-0/32", body: "x", wantRanged: true},
		{name: "missing header", body: "x"},
		{name: "wrong start", contentRange: "bytes 1-1/32", body: "x"},
		{name: "wrong end", contentRange: "bytes 0-1/32", body: "xy"},
		{name: "unknown total", contentRange: "bytes 0-0/*", body: "x"},
		{name: "extra body", contentRange: "bytes 0-0/32", body: "xy"},
		{name: "empty body", contentRange: "bytes 0-0/32"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") != "bytes=0-0" {
					t.Errorf("unexpected probe range: %q", r.Header.Get("Range"))
				}
				w.Header().Set("Content-Range", tt.contentRange)
				w.WriteHeader(http.StatusPartialContent)
				flushReviewDownloadResponse(t, w)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			info, conclusive := newTestGitHubReleaseClient().probeDownloadRangeOnce(context.Background(), srv.URL)
			require.True(t, conclusive)
			require.Equal(t, tt.wantRanged, info.ranged)
			if tt.wantRanged {
				require.Equal(t, int64(32), info.size)
			}
		})
	}
}

func TestDownloadSegmentOnceRejectsMismatchedRangeBeforeWriting(t *testing.T) {
	tests := []struct {
		name          string
		contentRange  string
		contentLength string
	}{
		{name: "missing header"},
		{name: "wrong start", contentRange: "bytes 0-7/32"},
		{name: "wrong end", contentRange: "bytes 8-16/32"},
		{name: "wrong total", contentRange: "bytes 8-15/33"},
		{name: "trailing data", contentRange: "bytes 8-15/32 junk"},
		{name: "wrong length", contentRange: "bytes 8-15/32", contentLength: "9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Range", tt.contentRange)
				if tt.contentLength != "" {
					w.Header().Set("Content-Length", tt.contentLength)
				}
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write([]byte("12345678"))
			}))
			defer srv.Close()
			dest := filepath.Join(t.TempDir(), "segments.bin")
			original := bytes.Repeat([]byte{0xcc}, 32)
			require.NoError(t, os.WriteFile(dest, original, 0600))
			out, err := os.OpenFile(dest, os.O_RDWR, 0)
			require.NoError(t, err)
			defer func() { _ = out.Close() }()
			written, err := newTestGitHubReleaseClient().downloadSegmentOnce(context.Background(), srv.URL, out, 8, 15, 32)
			require.ErrorIs(t, err, errInvalidDownloadRange)
			require.Zero(t, written)
			got, err := os.ReadFile(dest)
			require.NoError(t, err)
			require.Equal(t, original, got, "rejected headers must not modify any segment")
		})
	}
}

func TestDownloadSegmentOnceBoundsWritesForOverlongBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 8-15/32")
		w.WriteHeader(http.StatusPartialContent)
		flushReviewDownloadResponse(t, w)
		_, _ = w.Write([]byte("12345678"))
		flushReviewDownloadResponse(t, w)
		_, _ = w.Write([]byte("EXTRA DATA MUST NOT REACH ADJACENT SEGMENTS"))
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "segments.bin")
	original := bytes.Repeat([]byte{0xcc}, 32)
	require.NoError(t, os.WriteFile(dest, original, 0600))
	out, err := os.OpenFile(dest, os.O_RDWR, 0)
	require.NoError(t, err)
	defer func() { _ = out.Close() }()
	written, err := newTestGitHubReleaseClient().downloadSegmentOnce(context.Background(), srv.URL, out, 8, 15, 32)
	require.True(t, errors.Is(err, errInvalidDownloadRange), "expected rejected extra body, got %v", err)
	require.LessOrEqual(t, written, int64(8))
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Len(t, got, len(original), "a segment must not extend the file")
	require.Equal(t, original[:8], got[:8])
	require.Equal(t, original[16:], got[16:], "extra data must not overwrite the next segment")
}

func TestDownloadFileFallsBackAfterInvalidRangeResponse(t *testing.T) {
	for _, failure := range []string{"missing header", "wrong start", "wrong end", "changed total", "extra body"} {
		t.Run(failure, func(t *testing.T) {
			payload := make([]byte, 384)
			for i := range payload {
				payload[i] = byte(i*37 + i/251)
			}
			var fallbacks int32
			var segmentRequests int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rangeHeader := r.Header.Get("Range")
				if rangeHeader == "" {
					atomic.AddInt32(&fallbacks, 1)
					http.ServeContent(w, r, "asset.tar.gz", time.Time{}, bytes.NewReader(payload))
					return
				}
				if rangeHeader == "bytes=0-0" {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(payload)))
					w.WriteHeader(http.StatusPartialContent)
					_, _ = w.Write(payload[:1])
					return
				}
				atomic.AddInt32(&segmentRequests, 1)
				var start, end int
				if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || end >= len(payload) {
					http.Error(w, "unexpected range", http.StatusBadRequest)
					return
				}
				contentRange := fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload))
				switch failure {
				case "missing header":
					contentRange = ""
				case "wrong start":
					contentRange = fmt.Sprintf("bytes %d-%d/%d", start+1, end, len(payload))
				case "wrong end":
					contentRange = fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(payload))
				case "changed total":
					contentRange = fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)+1)
				}
				w.Header().Set("Content-Range", contentRange)
				w.WriteHeader(http.StatusPartialContent)
				flushReviewDownloadResponse(t, w)
				_, _ = w.Write(payload[start : end+1])
				if failure == "extra body" {
					flushReviewDownloadResponse(t, w)
					_, _ = w.Write([]byte{0xff})
				}
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			dest := filepath.Join(t.TempDir(), "asset.tar.gz")
			err := newTestGitHubReleaseClient().DownloadFile(ctx, srv.URL, dest, int64(len(payload)))
			require.NoError(t, err)
			got, err := os.ReadFile(dest)
			require.NoError(t, err)
			require.Equal(t, payload, got)
			require.Positive(t, atomic.LoadInt32(&segmentRequests), "test must reach the parallel path")
			require.Equal(t, int32(1), atomic.LoadInt32(&fallbacks), "invalid range response must trigger a whole-file retry")
		})
	}
}

func flushReviewDownloadResponse(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	flusher, ok := w.(http.Flusher)
	if !ok {
		t.Error("test server response must support streaming flushes")
		return
	}
	flusher.Flush()
}
