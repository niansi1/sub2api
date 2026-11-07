package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type githubReleaseClient struct {
	httpClient         *http.Client
	downloadHTTPClient *http.Client
	updateGitHubToken  string
}

type githubReleaseClientError struct {
	err error
}

// NewGitHubReleaseClient 创建 GitHub Release 客户端
// proxyURL 为空时直连 GitHub，支持 http/https/socks5/socks5h 协议
// 代理配置失败时行为由 allowDirectOnProxyError 控制：
//   - false（默认）：返回错误占位客户端，禁止回退到直连
//   - true：回退到直连（仅限管理员显式开启）
func NewGitHubReleaseClient(proxyURL string, allowDirectOnProxyError bool) service.GitHubReleaseClient {
	// 安全说明：httpclient.GetClient 的错误链（url.Parse / proxyutil）不含明文代理凭据，
	// 但仍通过 slog 仅在服务端日志记录，不会暴露给 HTTP 响应。
	sharedClient, err := httpclient.GetClient(httpclient.Options{
		Timeout:  30 * time.Second,
		ProxyURL: proxyURL,
	})
	if err != nil {
		if strings.TrimSpace(proxyURL) != "" && !allowDirectOnProxyError {
			slog.Warn("proxy client init failed, all requests will fail", "service", "github_release", "error", err)
			return &githubReleaseClientError{err: fmt.Errorf("proxy client init failed and direct fallback is disabled; set security.proxy_fallback.allow_direct_on_error=true to allow fallback: %w", err)}
		}
		sharedClient = &http.Client{Timeout: 30 * time.Second}
	}
	apiClient := cloneHTTPClient(sharedClient)
	apiClient.CheckRedirect = githubAPICheckRedirect(apiClient.CheckRedirect)

	// 下载客户端需要更长的超时时间：跨境链路上单个连接可能只有十几 KB/s，
	// 因此改为分段并发下载（见 DownloadFile），并给出足够长的总超时。
	downloadClient, err := httpclient.GetClient(httpclient.Options{
		Timeout:  30 * time.Minute,
		ProxyURL: proxyURL,
	})
	if err != nil {
		if strings.TrimSpace(proxyURL) != "" && !allowDirectOnProxyError {
			slog.Warn("proxy download client init failed, all requests will fail", "service", "github_release", "error", err)
			return &githubReleaseClientError{err: fmt.Errorf("proxy client init failed and direct fallback is disabled; set security.proxy_fallback.allow_direct_on_error=true to allow fallback: %w", err)}
		}
		downloadClient = &http.Client{Timeout: 30 * time.Minute}
	}
	downloadClient = cloneHTTPClient(downloadClient)

	return &githubReleaseClient{
		httpClient:         apiClient,
		downloadHTTPClient: downloadClient,
		updateGitHubToken:  os.Getenv("UPDATE_GITHUB_TOKEN"),
	}
}

func cloneHTTPClient(client *http.Client) *http.Client {
	cloned := *client
	return &cloned
}

func isGitHubAPIURL(url *url.URL) bool {
	return url != nil && strings.EqualFold(url.Scheme, "https") && url.User == nil &&
		strings.EqualFold(url.Host, "api.github.com")
}

func githubAPICheckRedirect(previous func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if !isGitHubAPIURL(req.URL) {
			req.Header.Del("Authorization")
		}
		if previous != nil {
			return previous(req, via)
		}
		return nil
	}
}

func (c *githubReleaseClient) newAPIRequest(ctx context.Context, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "Sub2API-Updater")
	if c.updateGitHubToken != "" && isGitHubAPIURL(req.URL) {
		req.Header.Set("Authorization", "Bearer "+c.updateGitHubToken)
	}
	return req, nil
}

func (c *githubReleaseClientError) FetchLatestRelease(ctx context.Context, repo string) (*service.GitHubRelease, error) {
	return nil, c.err
}

func (c *githubReleaseClientError) FetchRecentReleases(ctx context.Context, repo string, perPage int) ([]*service.GitHubRelease, error) {
	return nil, c.err
}

func (c *githubReleaseClientError) DownloadFile(ctx context.Context, url, dest string, maxSize int64) error {
	return c.err
}

func (c *githubReleaseClientError) FetchChecksumFile(ctx context.Context, url string) ([]byte, error) {
	return nil, c.err
}

func (c *githubReleaseClient) FetchLatestRelease(ctx context.Context, repo string) (*service.GitHubRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)

	req, err := c.newAPIRequest(ctx, url)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release service.GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, err
	}

	return &release, nil
}

func (c *githubReleaseClient) FetchRecentReleases(ctx context.Context, repo string, perPage int) ([]*service.GitHubRelease, error) {
	if perPage <= 0 {
		perPage = 10
	}
	if perPage > 100 {
		perPage = 100 // GitHub API hard limit
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=%d", repo, perPage)

	req, err := c.newAPIRequest(ctx, url)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var releases []*service.GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, err
	}

	return releases, nil
}

// Download tuning. GitHub serves release assets from a CDN that rate-limits
// each individual connection (measured ~12-33 KB/s from some regions), so a
// single-stream transfer of a ~40 MB archive can exceed the caller's download
// timeout by several times. Byte-range downloads over a handful of connections
// measured ~300 KB/s for the same asset, which brings the transfer back inside
// the timeout budget. Every fallback below degrades to the plain single-stream
// path so nothing breaks on servers that do not honour Range requests.
const (
	// downloadSegmentParallelism is the number of concurrent range requests.
	// Measured on a host where the CDN throttles each connection to ~13 KB/s:
	// throughput scales linearly with the number of connections, so this is
	// what keeps a ~40 MB archive inside the update timeout budget.
	downloadSegmentParallelism = 24
	// downloadSegmentAttempts bounds how often a stalled segment is resumed.
	// Every attempt keeps the bytes already written, so retries are cheap and a
	// generous count lets a segment sit out a multi-minute link outage.
	downloadSegmentAttempts = 20
	// downloadSegmentAttemptTimeout caps a single segment attempt. Without it a
	// black-holed TCP connection would block until the download client timeout,
	// stalling the whole transfer even though every other segment finished.
	downloadSegmentAttemptTimeout = 2 * time.Minute
	// downloadRangeProbeTimeout bounds a single Range capability probe.
	downloadRangeProbeTimeout = 15 * time.Second
	// downloadProbeBudget bounds how long the probe keeps retrying. The link to
	// GitHub is intermittently blocked on some hosts - not merely slow - so a
	// fixed attempt count would give up during an outage that would have
	// recovered a minute later. Failed dials return immediately, which makes a
	// long wall-clock budget cheap.
	downloadProbeBudget = 8 * time.Minute
	// downloadProbeRetryDelay is the pause between probe attempts.
	downloadProbeRetryDelay = 2 * time.Second
	// downloadSegmentBackoffMax caps the per-segment retry backoff.
	downloadSegmentBackoffMax = 20 * time.Second
)

// errInvalidDownloadRange marks protocol errors that cannot be repaired by
// resuming the same segment. The caller falls back to a bounded whole-file GET.
var errInvalidDownloadRange = errors.New("invalid range response")

type downloadContentRange struct {
	start int64
	end   int64
	total int64
}

// parseDownloadContentRange accepts one complete byte range with a known size.
// Wildcards, multiple ranges and trailing data cannot describe a segment safely.
func parseDownloadContentRange(value string) (downloadContentRange, error) {
	invalid := func() (downloadContentRange, error) {
		return downloadContentRange{}, fmt.Errorf("invalid Content-Range %q", value)
	}
	unit, bounds, ok := strings.Cut(strings.TrimSpace(value), " ")
	if !ok || !strings.EqualFold(unit, "bytes") {
		return invalid()
	}
	span, size, ok := strings.Cut(bounds, "/")
	if !ok {
		return invalid()
	}
	first, last, ok := strings.Cut(span, "-")
	if !ok {
		return invalid()
	}
	var numbers [3]int64
	for i, part := range []string{first, last, size} {
		if part == "" {
			return invalid()
		}
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return invalid()
			}
		}
		number, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return invalid()
		}
		numbers[i] = number
	}
	if numbers[0] > numbers[1] || numbers[1] >= numbers[2] {
		return invalid()
	}
	return downloadContentRange{start: numbers[0], end: numbers[1], total: numbers[2]}, nil
}

// downloadRangeInfo describes what the origin told us about the asset.
type downloadRangeInfo struct {
	size   int64 // total number of bytes, only valid when ranged is true
	ranged bool  // true when the origin honoured a Range request
}

func (c *githubReleaseClient) DownloadFile(ctx context.Context, url, dest string, maxSize int64) error {
	info := c.probeDownloadRange(ctx, url)
	if info.ranged && info.size > 0 {
		err := c.downloadSegmented(ctx, url, dest, info.size, maxSize)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		// Some networks answer the probe but refuse the parallel transfer.
		// Fall through to the well-tested single-stream path.
	}
	return c.downloadSingleStream(ctx, url, dest, maxSize)
}

// probeDownloadRange requests a single byte to learn whether Range requests are
// supported and how large the asset is. Transport failures are retried because
// the upstream link is intermittent on some hosts; only an explicit HTTP answer
// is treated as authoritative.
func (c *githubReleaseClient) probeDownloadRange(ctx context.Context, rawURL string) downloadRangeInfo {
	deadline := time.Now().Add(downloadProbeBudget)
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return downloadRangeInfo{}
			case <-time.After(downloadProbeRetryDelay):
			}
		}
		info, ok := c.probeDownloadRangeOnce(ctx, rawURL)
		if ok {
			return info
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return downloadRangeInfo{}
		}
	}
}

// probeDownloadRangeOnce performs one probe. The second result reports whether
// the outcome is conclusive (the origin answered), so the caller can stop
// retrying on a definite "no Range support" response.
func (c *githubReleaseClient) probeDownloadRangeOnce(ctx context.Context, rawURL string) (downloadRangeInfo, bool) {
	probeCtx, cancel := context.WithTimeout(ctx, downloadRangeProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		// A malformed URL cannot heal on retry.
		return downloadRangeInfo{}, true
	}
	req.Header.Set("Range", "bytes=0-0")

	resp, err := c.downloadHTTPClient.Do(req)
	if err != nil {
		return downloadRangeInfo{}, false
	}
	defer func() { _ = resp.Body.Close() }()

	// Definitive answers that retrying cannot change: the origin served the
	// whole file (no Range support, use the single-stream path) or rejected the
	// request outright (404/403/...). Fall back instead of retrying.
	if resp.StatusCode == http.StatusOK || (resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests) {
		return downloadRangeInfo{}, true
	}
	// Transient HTTP/transport failures retain the existing probe retry budget.
	if resp.StatusCode != http.StatusPartialContent {
		return downloadRangeInfo{}, false
	}

	contentRange, err := parseDownloadContentRange(resp.Header.Get("Content-Range"))
	if err != nil || contentRange.start != 0 || contentRange.end != 0 {
		return downloadRangeInfo{}, true
	}
	if resp.ContentLength >= 0 && resp.ContentLength != 1 {
		return downloadRangeInfo{}, true
	}
	// The probe itself must contain exactly the one byte it advertised. Read
	// one extra byte to detect a server that sends the whole asset as a 206.
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 2))
	if err != nil {
		return downloadRangeInfo{}, false
	}
	if n != 1 {
		return downloadRangeInfo{}, true
	}
	return downloadRangeInfo{size: contentRange.total, ranged: true}, true
}

// downloadSegmented fetches the asset as contiguous byte ranges in parallel.
func (c *githubReleaseClient) downloadSegmented(ctx context.Context, rawURL, dest string, total, maxSize int64) error {
	if total <= 0 {
		return fmt.Errorf("invalid download size: %d", total)
	}
	if total > maxSize {
		return fmt.Errorf("file too large: %d bytes (max %d)", total, maxSize)
	}

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	abort := func(err error) error {
		_ = out.Close()
		_ = os.Remove(dest) // Clean up partial file (best-effort)
		return err
	}
	if err := out.Truncate(total); err != nil {
		return abort(err)
	}

	workers := int64(downloadSegmentParallelism)
	if workers > total {
		workers = total
	}
	segmentSize := (total + workers - 1) / workers

	segCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	for start := int64(0); start < total; start += segmentSize {
		end := start + segmentSize - 1
		if end >= total {
			end = total - 1
		}
		wg.Add(1)
		go func(start, end int64) {
			defer wg.Done()
			if err := c.downloadSegment(segCtx, rawURL, out, start, end, total); err != nil {
				errOnce.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}(start, end)
	}
	wg.Wait()

	if firstErr != nil {
		return abort(firstErr)
	}
	info, err := out.Stat()
	if err != nil {
		return abort(err)
	}
	if info.Size() != total {
		return abort(fmt.Errorf("download size mismatch: got %d bytes, expected %d", info.Size(), total))
	}
	if err := out.Sync(); err != nil {
		return abort(err)
	}
	return out.Close()
}

// downloadSegment fetches bytes [start, end] and resumes after a dropped
// connection instead of discarding the whole segment.
func (c *githubReleaseClient) downloadSegment(ctx context.Context, rawURL string, out *os.File, start, end, total int64) error {
	offset := start
	var lastErr error
	for attempt := 0; attempt < downloadSegmentAttempts && offset <= end; attempt++ {
		if attempt > 0 {
			// Exponential backoff so a segment can outlast a short outage of
			// the link instead of burning its attempts in a few seconds.
			delay := time.Second << uint(attempt-1)
			if delay > downloadSegmentBackoffMax {
				delay = downloadSegmentBackoffMax
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		written, err := c.downloadSegmentOnce(ctx, rawURL, out, offset, end, total)
		offset += written
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			if offset > end {
				return nil
			}
			err = io.ErrUnexpectedEOF
		}
		// Invalid ranges and extra data must fail even when all requested
		// bytes were already written; otherwise offset alone could hide them.
		if errors.Is(err, errInvalidDownloadRange) || offset > end {
			return err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("incomplete range download: bytes %d-%d stopped at %d", start, end, offset)
	}
	return lastErr
}

func (c *githubReleaseClient) downloadSegmentOnce(ctx context.Context, rawURL string, out *os.File, start, end, total int64) (int64, error) {
	// Bound each attempt so a stalled connection cannot hold the transfer open
	// until the (much longer) download client timeout fires.
	attemptCtx, cancel := context.WithTimeout(ctx, downloadSegmentAttemptTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	resp, err := c.downloadHTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("range request returned %d", resp.StatusCode)
	}
	contentRange, err := parseDownloadContentRange(resp.Header.Get("Content-Range"))
	if err != nil {
		return 0, fmt.Errorf("%w: %v", errInvalidDownloadRange, err)
	}
	if contentRange.start != start || contentRange.end != end || contentRange.total != total {
		return 0, fmt.Errorf("%w: Content-Range %q does not match bytes %d-%d/%d", errInvalidDownloadRange, resp.Header.Get("Content-Range"), start, end, total)
	}
	expected := end - start + 1
	if resp.ContentLength >= 0 && resp.ContentLength != expected {
		return 0, fmt.Errorf("%w: Content-Length %d does not match %d bytes", errInvalidDownloadRange, resp.ContentLength, expected)
	}

	buf := make([]byte, 128*1024)
	var written int64
	for {
		// Read at most the remaining range plus one extra byte. Extra data is
		// rejected before WriteAt, so it cannot overwrite another segment or
		// extend the preallocated file beyond the checked total/maxSize.
		remaining := expected - written
		readSize := int64(len(buf))
		if remaining < readSize {
			readSize = remaining + 1
		}
		n, readErr := resp.Body.Read(buf[:int(readSize)])
		if int64(n) > remaining {
			return written, fmt.Errorf("%w: body exceeds bytes %d-%d", errInvalidDownloadRange, start, end)
		}
		if n > 0 {
			// WriteAt does not move the shared file offset, so every segment can
			// write concurrently through the same file handle.
			nWritten, writeErr := out.WriteAt(buf[:n], start+written)
			written += int64(nWritten)
			if writeErr != nil {
				return written, writeErr
			}
			if nWritten != n {
				return written, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				if written != expected {
					return written, io.ErrUnexpectedEOF
				}
				return written, nil
			}
			return written, readErr
		}
	}
}

// downloadSingleStream is the original whole-file download used when the origin
// does not support Range requests.
func (c *githubReleaseClient) downloadSingleStream(ctx context.Context, url, dest string, maxSize int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	// 使用预配置的下载客户端（已包含代理配置）
	resp, err := c.downloadHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned %d", resp.StatusCode)
	}

	// SECURITY: Check Content-Length if available
	if resp.ContentLength > maxSize {
		return fmt.Errorf("file too large: %d bytes (max %d)", resp.ContentLength, maxSize)
	}

	out, err := os.Create(dest)
	if err != nil {
		return err
	}

	// SECURITY: Use LimitReader to enforce max download size even if Content-Length is missing/wrong
	limited := io.LimitReader(resp.Body, maxSize+1)
	written, err := io.Copy(out, limited)

	// Close file before attempting to remove (required on Windows)
	_ = out.Close()

	if err != nil {
		_ = os.Remove(dest) // Clean up partial file (best-effort)
		return err
	}

	// Check if we hit the limit (downloaded more than maxSize)
	if written > maxSize {
		_ = os.Remove(dest) // Clean up partial file (best-effort)
		return fmt.Errorf("download exceeded maximum size of %d bytes", maxSize)
	}

	return nil
}

func (c *githubReleaseClient) FetchChecksumFile(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}
