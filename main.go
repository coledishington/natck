package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
)

type connection struct {
	client  *http.Client
	url     *url.URL
	nextUrl *url.URL
	closed  bool
}

func readUrls(input io.Reader) ([]url.URL, error) {
	urls := make([]url.URL, 0)
	b := make([]byte, 0)
	for {
		_, err := input.Read(b)
		if err == io.EOF {
			break
		}
		if err != nil {
			err = fmt.Errorf("failed to read url line: %w", err)
			return nil, err
		}

		u, err := url.Parse(string(b))
		if err != nil {
			err = fmt.Errorf("failed to read url line: %w", err)
			return nil, err
		}

		urls = append(urls, *u)
	}
	return urls, nil
}

func scrapUrl(client *http.Client, host *url.URL) ([]url.URL, error) {
	resp, err := client.Get(host.String())
	if err != nil {
		err = fmt.Errorf("failed get uri %v: %w", host.String(), err)
		return nil, err
	}
	defer resp.Body.Close()

	urls := Scrap(host, resp.Body)
	return urls, nil
}

func makeClient() *http.Client {
	// Need a unique transport per http.Client to avoid re-using the same
	// connections, otherwise the NAT count will be wrong.
	defaultTransport := http.DefaultTransport.(*http.Transport)
	client := http.Client{
		Transport: &http.Transport{
			Proxy:                 defaultTransport.Proxy,
			DialContext:           defaultTransport.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          0,
			IdleConnTimeout:       defaultTransport.IdleConnTimeout,
			TLSHandshakeTimeout:   defaultTransport.TLSHandshakeTimeout,
			ExpectContinueTimeout: defaultTransport.ExpectContinueTimeout,
		},
	}
	return &client
}

func makeConnection(h *url.URL) *connection {
	c := &connection{
		client:  makeClient(),
		url:     h,
		nextUrl: h,
	}
	return c
}

func scrapConnection(c *connection) {
	sUrls, err := scrapUrl(c.client, c.nextUrl)
	if err != nil {
		c.closed = true
		return
	}

	// Find uri from same host, re-use original uri if no new uri
	// was not scraped
	for j := range sUrls {
		sUrl := &sUrls[j]
		if c.url.Host == sUrl.Host {
			c.nextUrl = sUrl
			break
		}
	}
}

func scrapConnections(conns []*connection) {
	for i := range conns {
		c := conns[i]
		if c.closed {
			continue
		}
		scrapConnection(c)
	}
}

func MeasureMaxConnections(urls []url.URL) int {
	http.DefaultTransport.(*http.Transport).MaxIdleConns = 0

	connections := make([]*connection, 0)
	for i := range urls {
		c := makeConnection(&urls[i])
		connections = append(connections, c)
	}

	scrapConnections(connections)
	scrapConnections(connections)

	nConns := 0
	for i := range connections {
		c := connections[i]
		if c.closed {
			continue
		}
		nConns++
	}

	return nConns
}

func main() {
	fmt.Println("Measure max connections! Please provide a path to a url list")
	for {
		var path string

		_, err := fmt.Scanln(&path)
		if err != nil {
			fmt.Println("Failed to read input file path:", err)
			break
		}

		f, err := os.Open(path)
		if err != nil {
			fmt.Printf("Could not open %v: %v", path, err)
			break
		}

		urls, err := readUrls(f)
		if err != nil {
			fmt.Printf("Failed to read urls: %v: %v", path, err)
			break
		}

		nConns := MeasureMaxConnections(urls)
		fmt.Println("Max connections is", nConns)
		fmt.Println()
	}
}
