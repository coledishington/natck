# natck

natck's (nat-check's) purpose is a utility for measuring the number of
concurrent connections a NAT will allow, most useful for NATs outside
of your control, like CGNAT. The connections aim to look like real traffic
to avoid dropping or quick timeouts to get an accurate measurement.

* Uses realistic "looking" http traffic.
* Maximises the variability of crawled sites.
* Dynamically discovers new hosts to crawl.
* Respects servers via robots.txt and HTTP 429 rate-limit.

# Basic Usage

The natck utility accepts a list of urls for the remote servers. For the best
experience, specify a list of 10+ urls.

    example1.com
    example2.com

Once the url list is assembled it can be piped to natck like

    cat url-list.txt | ./natck

The natck utility bootstraps itself from the input list of urls,
dynamically finding new hosts. For this reason, natck can be
started from a single url but will finish much faster when passed
a larger number of urls.

# Building natck

Use the usual golang tools like

    go build

and

    go run

# Contributors

Before submitting any patches, please run <code>go fmt</code> and <code>go vet</code> over each commit. Feel free to open a discussion to communicate any feature ideas if you're unsure how to implement or fit it into the surrounding code.

# License

natck is licensed under [GPLv2](COPYING).
