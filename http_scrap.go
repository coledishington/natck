// Functions related to requesting resources from HTTP servers and scraping the result.
package main

import (
	"fmt"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"slices"
	"sync"
	"time"
)

type host struct {
	ip        netip.AddrPort
	hostnames []string
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

func getUrl(client *http.Client, target *url.URL) (netip.AddrPort, *http.Response, error) {
	remoteAddr := netip.AddrPortFrom(netip.IPv4Unspecified(), 0)
	targetUrl := target.String()
	req, err := http.NewRequest(http.MethodGet, targetUrl, nil)
	if err != nil {
		err = fmt.Errorf("failed to make request: %w", err)
		return remoteAddr, nil, err
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		ConnectDone: func(network, addr string, _ error) {
			rAddr, err := netip.ParseAddrPort(addr)
			if err != nil {
				return
			}
			remoteAddr = rAddr
		},
	}))
	resp, err := client.Do(req)
	if err != nil {
		err = fmt.Errorf("failed get uri %v: %w", targetUrl, err)
		return remoteAddr, nil, err
	}
	return remoteAddr, resp, err
}

func scrapResponse(target *url.URL, resp *http.Response) []*url.URL {
	// Parse urls from content
	urls := Scrap(target, resp.Body)

	// Add url from redirect if it belongs to the same server
	location, err := resp.Location()
	if err == nil && !sliceContainsUrl(urls, location) {
		urls = append(urls, location)
	}
	return urls
}

func scrapHostUrl(client *http.Client, host *host, target *url.URL) ([]*url.URL, error) {
	remoteIP, resp, err := getUrl(client, target)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Use the new IP if a new connection was made
	if !remoteIP.Addr().IsUnspecified() {
		host.ip = remoteIP
	}

	return scrapResponse(target, resp), nil
}

func scrapConnection(r *roundtrip) *roundtrip {
	r.requestTs = time.Now()
	sUrls, err := scrapHostUrl(r.client, r.host, r.url)
	r.replyTs = time.Now()
	r.failed = err != nil
	r.scrapedUrls = sUrls
	return r
}

func scrapConnections(wg *sync.WaitGroup, pendingScraps <-chan *roundtrip, scraped chan<- *roundtrip, cancel <-chan struct{}) {
	defer wg.Done()

	for {
		var r *roundtrip
		var ok bool

		// Quit without possibility of selecting other cases */
		select {
		case <-cancel:
			return
		default:
		}

		// Stop blocking if canceled */
		select {
		case r, ok = <-pendingScraps:
		case <-cancel:
			return
		}
		if !ok {
			break
		}

		select {
		case scraped <- scrapConnection(r):
		case <-cancel:
			return
		}
	}
}
