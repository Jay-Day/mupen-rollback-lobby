//go:build !ratelimit_loopback

package main

// limitLoopback: the request limits leave loopback addresses alone, so
// several local clients can test freely. Only a build with the
// ratelimit_loopback tag (loopback_limited.go) changes it.
const limitLoopback = false
