package main

import (
	"bufio"
	"io"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	userAgent      = "User-agent"
	ruleAllow      = "Allow"
	ruleDisallow   = "Disallow"
	ruleCrawlDelay = "Crawl-delay"
)

type robotstxt map[string][]string

func (r robotstxt) crawlDelay() (time.Duration, bool) {
	s, found := r[ruleCrawlDelay]
	if !found {
		return 0, false
	}

	n, err := time.ParseDuration(s[0])
	if err != nil {
		return 0, false
	}

	return n, true
}

func (r robotstxt) pathAllowed(path string) bool {
	disallowed, found := r[ruleDisallow]
	if !found {
		return true
	}

	i := IndexPathPrefix(disallowed, path)
	if i == -1 {
		return true
	}

	allowed, found := r[ruleAllow]
	if !found {
		return false
	}

	j := IndexPathPrefix(allowed, path)
	if j == -1 {
		return false
	}

	// Only non-conflicting paths are parsed out of
	// robots.txt, hence the larger prefix must have
	// appeared first.
	return len(disallowed[i]) < len(allowed[j])
}

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

func IndexPathPrefix(paths []string, value string) int {
	return slices.IndexFunc(paths, func(p string) bool {
		return strings.HasPrefix(value, p)
	})
}

func parseCrawlDelay(value string) (time.Duration, error) {
	delayTime, err := time.ParseDuration(value)
	if err == nil {
		return delayTime, nil
	}

	// Assume seconds if value has no unit
	return time.ParseDuration(value + "s")
}

func scrapRobotsTxt(input io.Reader) robotstxt {
	rules := map[string][]string{}

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

		if token == userAgent {
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

		if token == ruleCrawlDelay {
			// First Crawl-delay is accepted, similar to Allow and Disallow
			if len(rules[ruleCrawlDelay]) > 0 {
				continue
			}

			delayTime, err := parseCrawlDelay(value)
			if err != nil {
				continue
			}
			rules[ruleCrawlDelay] = []string{delayTime.String()}
		} else if token == ruleAllow || token == ruleDisallow {
			value, err := url.PathUnescape(value)
			if err != nil {
				continue
			}

			// robots.txt uses the first matching rule. Don't add paths that
			// will never be used
			if IndexPathPrefix(rules[ruleAllow], value) != -1 {
				continue
			}
			if IndexPathPrefix(rules[ruleDisallow], value) != -1 {
				continue
			}
			rules[token] = append(rules[token], value)
		}
	}

	io.ReadAll(input)
	return rules
}
