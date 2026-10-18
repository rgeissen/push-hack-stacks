module github.com/rgeissen/push-hack-stacks

go 1.25.0

require github.com/federico-pepe/ableton-push-hack/core v0.2.1

require (
	golang.org/x/image v0.41.0 // indirect
	golang.org/x/text v0.37.0 // indirect
)

// TEMPORARY, until federico-pepe/ableton-push-hack#33, #36 (live scale),
// #37 (listen) and #38 (Push Hack Home) are merged and tagged: core from
// #38's branch on the fork. Replace with the new core/vX.Y.Z tag and delete
// this (framework rule: a tag, no replace).
replace github.com/federico-pepe/ableton-push-hack/core => github.com/rgeissen/ableton-push-hack/core v0.0.0-20261018105801-870969d14c05
