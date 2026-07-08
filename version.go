package main

import "fmt"

// version is set at build time via -ldflags "-X main.version=...".
// Defaults to "dev" for local `go build` / `go run`.
var version = "dev"

// printVersion is invoked when the user runs `agent-inbox version`.
func printVersion() {
	fmt.Printf("agent-inbox %s\n", version)
}
