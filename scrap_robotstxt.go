package main

import (
	"bufio"
	"io"
	"strings"
	"time"
)

func isRuleLine(line string) bool {
	return strings.Contains(line, ":")
}

func findUserAgent(line string) (string, bool) {
	value, found := strings.CutPrefix(line, "User-agent:")
	if !found {
		return "", false
	}

	value, _, _ = strings.Cut(value, "#")
	value = strings.TrimSpace(value)
	return value, true
}

func findCrawlDelay(line string) (string, bool) {
	value, found := strings.CutPrefix(line, "Crawl-delay:")
	if !found {
		return "", false
	}

	value, _, _ = strings.Cut(value, "#")
	value = strings.TrimSpace(value)
	return value, true
}

func scrapRobotsTxt(input io.Reader) (time.Duration, bool) {
	skipToNextValue := false
	matchingAgent := true
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if value, found := findUserAgent(line); found {
			if !skipToNextValue {
				matchingAgent = value == "*"
				if matchingAgent {
					skipToNextValue = true
				}
			}
			continue
		}
		if !matchingAgent || !isRuleLine(line) {
			continue
		}
		skipToNextValue = false

		delay, found := findCrawlDelay(line)
		if !found {
			continue
		}
		delayTime, err := time.ParseDuration(delay)
		if err != nil {
			continue
		}
		return delayTime, true
	}
	return 0, false
}
