//go:build ratelimit_loopback

package main

// limitLoopback: a test build (go build -tags ratelimit_loopback) applies the
// request limits, and their automatic ban, to loopback addresses too, so a
// local rig can trip them with clients on 127.0.0.x. Never deploy one: the
// release workflows build without the tag.
const limitLoopback = true
