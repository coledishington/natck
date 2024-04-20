package main

import (
	"bufio"
	"io"
	"strings"
	"time"
)

func tokenCase(s string) string {
	if len(s) == 0 {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

func splitTokenAndValue(s string) (string, string) {
	var token, value string

	s, _, _ = strings.Cut(s, "#")
	parts := strings.SplitN(s, ":", 2)
	if len(parts) >= 1 {
		token = tokenCase(strings.TrimSpace(parts[0]))
	}
	if len(parts) == 2 {
		value = strings.TrimSpace(parts[1])
	}
	return token, value
}

func parseCrawlDelay(value string) (time.Duration, error) {
	delayTime, err := time.ParseDuration(value)
	if err == nil {
		return delayTime, nil
	}
	// Assume seconds if value has no unit
	return time.ParseDuration(value + "s")
}

func scrapRobotsTxt(input io.Reader) (time.Duration, bool) {
	skipToNextValue := false
	matchingAgent := true
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		token, value := splitTokenAndValue(line)
		if token == "" || value == "" {
			continue
		}

		if token == "User-agent" {
			if !skipToNextValue {
				matchingAgent = value == "*"
				if matchingAgent {
					skipToNextValue = true
				}
			}
			continue
		}
		if !matchingAgent {
			continue
		}
		skipToNextValue = false

		if token != "Crawl-delay" {
			continue
		}

		delayTime, err := parseCrawlDelay(value)
		if err != nil {
			continue
		}
		return delayTime, true
	}
	return 0, false
}
