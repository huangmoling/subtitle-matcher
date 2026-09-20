//go:build windows

// net.go —— 带限速与重试的 HTTP 客户端
//
// 每个站点一个限速器：并发搜索时不会把同一站点打爆，
// 也不会因为多文件夹并行而触发对方的风控。
//
// 上下文按调用传入（而不是存在 getter 上），这样每个文件夹
// 可以有自己的超时；某个站点卡住时能单独掐断，不拖累整体。
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36"

const (
	acceptHTML = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
	acceptJSON = "application/json, text/plain, */*"
	acceptAny  = "*/*"
)

// rateLimiter 串行化并保证两次请求之间至少间隔 gap。
// 睡眠时持锁，所以并发调用会被排队，而不是各自睡各自。
type rateLimiter struct {
	mu   sync.Mutex
	last time.Time
	gap  time.Duration
}

func newRateLimiter(gap time.Duration) *rateLimiter {
	return &rateLimiter{gap: gap}
}

func (r *rateLimiter) wait(ctx context.Context) error {
	// 先在锁外等到大致可发的时刻，避免长时间占着锁
	r.mu.Lock()
	var sleep time.Duration
	if !r.last.IsZero() {
		if d := r.gap - time.Since(r.last); d > 0 {
			sleep = d
		}
	}
	r.last = time.Now().Add(sleep)
	r.mu.Unlock()

	if sleep <= 0 {
		return ctx.Err()
	}
	select {
	case <-time.After(sleep):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// httpGetter 面向单个站点的取数器
type httpGetter struct {
	client *http.Client
	lim    *rateLimiter
	tries  int // 单次请求的最大尝试次数
}

func newHTTPGetter(gap, timeout time.Duration, tries int) *httpGetter {
	if tries < 1 {
		tries = 1
	}
	return &httpGetter{
		client: &http.Client{Timeout: timeout},
		lim:    newRateLimiter(gap),
		tries:  tries,
	}
}

func buildRequest(ctx context.Context, rawURL, referer, accept string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	if accept == "" {
		accept = acceptHTML
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	return req, nil
}

// Get 取回正文，失败自动重试。
func (h *httpGetter) Get(ctx context.Context, rawURL, referer, accept string) ([]byte, error) {
	return h.getN(ctx, rawURL, referer, accept, h.tries)
}

func (h *httpGetter) getN(ctx context.Context, rawURL, referer, accept string, attempts int) ([]byte, error) {
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 1; i <= attempts; i++ {
		if err := h.lim.wait(ctx); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		req, err := buildRequest(ctx, rawURL, referer, accept)
		if err != nil {
			return nil, err
		}
		resp, err := h.client.Do(req)
		if err != nil {
			lastErr = err
		} else {
			body, rerr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
			resp.Body.Close()
			switch {
			case rerr != nil:
				lastErr = rerr
			case resp.StatusCode == http.StatusOK:
				return body, nil
			case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
				// 被风控了，重试没有意义，把正文带回去让上层判断原因
				return body, &statusError{code: resp.StatusCode, body: body}
			default:
				lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			}
		}
		if i < attempts {
			select {
			case <-time.After(time.Duration(i) * 700 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return nil, lastErr
}

// Head 只取响应头，用来补全文件大小（很轻，不下载正文）。
func (h *httpGetter) Head(ctx context.Context, rawURL, referer string) (int64, error) {
	if err := h.lim.wait(ctx); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", acceptAny)
	if referer != "" {
		req.Header.Set("Referer", referer)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength <= 0 {
		return 0, fmt.Errorf("未提供 Content-Length")
	}
	return resp.ContentLength, nil
}

// statusError 携带响应正文，方便上层识别站点的 JSON 错误说明
type statusError struct {
	code int
	body []byte
}

func (e *statusError) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

// Body 返回随错误一起拿到的响应正文
func (e *statusError) Body() []byte { return e.body }
