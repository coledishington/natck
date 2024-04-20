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

	// Add url from redirect if it belongs to the same server
	location, err := resp.Location()
	if err == nil && !sliceContainsUrl(urls, location) {
		urls = append(urls, location)
	}

	if strings.HasSuffix(r.url.String(), "robots.txt") {
		if crawlDelay, found := scrapRobotsTxt(resp.Body); found {
			r.crawlDelay = crawlDelay
		}
	} else if strings.HasSuffix(r.url.String(), ".html") {
		sUrls := ScrapHtml(r.url, resp.Body)
		urls = append(sUrls, urls...)
	} else {
		// Persistent connections need to have the body read
		io.ReadAll(resp.Body)
	}
	r.scrapedUrls = urls
	return r
}
