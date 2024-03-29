// Functions related to requesting resources from HTTP servers and scraping the result.
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
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
	scrapedUrls []*url.URL
	failed      bool
	requestTs   time.Time
	replyTs     time.Time
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

func scrapResponse(ctx context.Context, target *url.URL, resp *http.Response) []*url.URL {
	// Parse urls from content
	urls := Scrap(target, resp.Body)

	// Add url from redirect if it belongs to the same server
	location, err := resp.Location()
	if err == nil && !sliceContainsUrl(urls, location) {
		urls = append(urls, location)
	}
	return urls
}

func scrapHostUrl(ctx context.Context, client *http.Client, target *url.URL) ([]*url.URL, error) {
	resp, err := getUrl(ctx, client, target)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return scrapResponse(ctx, target, resp), nil
}

func scrapConnection(ctx context.Context, r *roundtrip) *roundtrip {
	r.requestTs = time.Now()
	sUrls, err := scrapHostUrl(ctx, r.client, r.url)
	r.replyTs = time.Now()
	r.failed = err != nil
	r.scrapedUrls = sUrls
	return r
}
