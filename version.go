package main

import (
	"fmt"
	"runtime/debug"
)

// version is set at build time via -ldflags "-X main.version=...", which is
// how goreleaser stamps a release. Empty for every other build.
var version = ""

// resolveVersion reports the most trustworthy version available.
//
// The ldflag wins when it is set. Without it, a binary from `go install
// <pkg>@<version>` still knows what it is — the module version is recorded in
// its build info, and reporting "dev" for one of those is wrong: the user
// installed a release, and would go on to file bugs against a version nobody
// can identify. Only a build from a working tree is genuinely "dev".
func resolveVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		// Go stamps this from VCS when building inside a repo, so a working
		// tree reports something like "v0.2.0+dirty" — which is a better
		// answer than "dev", not a worse one: it says which tag you are ahead
		// of and that you have uncommitted changes.
		//
		// "(devel)" is the placeholder when there is nothing to stamp from
		// (built outside a repo, or with -buildvcs=false). That is the one
		// case where "dev" is all that can honestly be said.
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}

// printVersion is invoked when the user runs `agent-inbox version`.
func printVersion() {
	fmt.Printf("agent-inbox %s\n", resolveVersion())
}
