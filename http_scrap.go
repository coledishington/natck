// Functions related to requesting resources from HTTP servers and scraping the result.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

type host struct {
	ip       netip.AddrPort
	hostPort string
}

type roundtrip struct {
	connId      uint
	client      *http.Client
	host        *host
	url         *url.URL
	err         error
	requestTs   time.Time
	replyTs     time.Time
	scrapedUrls []*url.URL
	crawlDelay  time.Duration
}

func sliceContainsUrl(urls []*url.URL, needle *url.URL) bool {
	return slices.ContainsFunc(urls, func(u *url.URL) bool {
		return *u == *needle
	})
}

func getUrl(ctx context.Context, client *http.Client, target *url.URL) (*http.Response, error) {
	targetUrl := target.String()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetUrl, nil)
	if err != nil {
		err = fmt.Errorf("failed to make request: %w", err)
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		err = fmt.Errorf("failed get uri %v: %w", targetUrl, err)
		return nil, err
	}
	return resp, err
}

func isReponseRobotstxt(resp *http.Response) bool {
	ctype := resp.Header["Content-Type"]
	isPlainText := len(ctype) > 0 &&
		strings.HasPrefix(ctype[0], "text/plain;")
	u := resp.Request.URL.String()
	return strings.HasSuffix(u, "robots.txt") && isPlainText
}

func isResponseHtml(resp *http.Response) bool {
	ctype := resp.Header["Content-Type"]
	if len(ctype) == 0 {
		return strings.HasSuffix(resp.Request.URL.String(), ".html")
	}

	return strings.HasPrefix(ctype[0], "text/html;")
}

func parseHttp429Headers(headers http.Header) (time.Duration, bool) {
	retryFields, found := headers["Retry-After"]
	if !found || len(retryFields) == 0 {
		return 0, false
	}

	retryS := retryFields[len(retryFields)-1]
	retry, err := strconv.Atoi(retryS)
	if err == nil {
		return time.Duration(retry) * time.Second, true
	}

	pattern := "Mon, 02 01 2006 03:04:05 MST"
	nextRetry, err := time.Parse(pattern, retryS)
	if err == nil {
		retry := time.Until(nextRetry)
		return retry, true
	}

	return 0, false
}

func scrapConnection(ctx context.Context, r *roundtrip) *roundtrip {
	var resp *http.Response

	r.requestTs = time.Now()
	resp, r.err = getUrl(ctx, r.client, r.url)
	r.replyTs = time.Now()
	if r.err != nil {
		return r
	}
	defer resp.Body.Close()

	urls := []*url.URL{}

	// Server requesting rate-limiting
	if resp.StatusCode == 429 {
		if retry, ok := parseHttp429Headers(resp.Header); ok {
			r.crawlDelay = max(retry, r.crawlDelay)
		} else {
			r.crawlDelay += time.Second
		}
	}

	// Add url from redirect if it belongs to the same server
	location, err := resp.Location()
	if err == nil && !sliceContainsUrl(urls, location) {
		urls = append(urls, location)
	}

	if isReponseRobotstxt(resp) {
		if crawlDelay, found := scrapRobotsTxt(resp.Body); found {
			r.crawlDelay = crawlDelay
		}
	} else if isResponseHtml(resp) {
		sUrls := ScrapHtml(r.url, resp.Body)
		urls = append(sUrls, urls...)
	} else {
		// Persistent connections need to have the body read
		io.ReadAll(resp.Body)
	}
	r.scrapedUrls = urls
	return r
}
