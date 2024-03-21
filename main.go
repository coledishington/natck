package main

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

func readUrls(input io.Reader) ([]*url.URL, error) {
	urls := make([]*url.URL, 0)
	r := bufio.NewReaderSize(input, 160)
	for more := true; more; {
		line, rErr := r.ReadString('\n')
		if rErr != nil && rErr != io.EOF {
			err := fmt.Errorf("failed to read url line: %w", rErr)
			return nil, err
		}
		more = rErr != io.EOF

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		u, err := url.Parse(line)
		if err != nil {
			continue
		}

		urls = append(urls, u)
	}
	return urls, nil
}

func main() {
	urls, err := readUrls(os.Stdin)
	if err != nil {
		fmt.Printf("Failed to read urls from stdin: %v", err)
		os.Exit(1)
	}

	nConns := MeasureMaxConnections(urls)
	fmt.Println("Max connections are", nConns)
}
