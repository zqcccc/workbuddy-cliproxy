//go:build !global

package main

// Default realm: the CN deployment at copilot.tencent.com.
// Build with `-tags global` to produce the workbuddy-global.so variant.
var (
	providerName = "workbuddy"
	buildRegion  = regionCN
)
