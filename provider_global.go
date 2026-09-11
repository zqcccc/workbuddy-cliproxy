//go:build global

package main

// Global realm: the international deployment at www.workbuddy.ai.
// Selected with `-tags global`; see build.sh.
var (
	providerName = "workbuddy-global"
	buildRegion  = regionGlobal
)
