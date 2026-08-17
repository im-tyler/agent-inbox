package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/im-tyler/agent-inbox/internal/board"
	"github.com/im-tyler/agent-inbox/internal/feed"
	"github.com/im-tyler/agent-inbox/internal/sources"
)

// runInbox is the reader: one list of everything waiting on you, merged from
// every configured teploy.inbox/v1 source.
//
// It shares nothing with the driver-based dashboard in the rest of this
// binary. That one steers live agent sessions by parsing their terminal
// output; this one only reads state other tools deliberately wrote down.
func runInbox(argv []string) {
	fs := flag.NewFlagSet("inbox", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the merged feed as JSON instead of opening the UI")
	all := fs.Bool("all", false, "also list sessions that are merely running")
	cfgPath := fs.String("sources", sources.ConfigPath(), "path to sources.json")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `agent-inbox inbox — what is waiting on you, across every source

Usage:
  agent-inbox inbox              what needs you
  agent-inbox inbox --all        everything, including sessions just running
  agent-inbox inbox --json       merged teploy.inbox/v1 feed on stdout

Sources come from %s. With no such file, the default is your local Claude Code
sessions plus teploy-ship if it is on PATH.

Flags:
`, sources.ConfigPath())
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		os.Exit(2)
	}

	cfg, err := sources.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sources: %v\n", err)
		os.Exit(1)
	}
	built, problems := cfg.BuildWithDiagnostics()
	// A source that was skipped for a typo used to vanish without a word, so
	// "kind": "opencdoe" looked exactly like an empty inbox.
	for _, p := range problems {
		fmt.Fprintf(os.Stderr, "! sources: %v\n", p)
	}
	if len(built) == 0 {
		fmt.Fprintf(os.Stderr, "no usable sources configured (%s)\n", *cfgPath)
		os.Exit(1)
	}

	if *asJSON {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		agg := sources.Fetch(ctx, built)
		// Unreachable sources go to stderr so stdout stays a clean feed for
		// whatever is parsing it.
		for _, bad := range sources.Errors(agg.Results) {
			fmt.Fprintf(os.Stderr, "! %v\n", bad.Err)
		}
		for _, r := range agg.Results {
			for _, w := range r.Warnings {
				fmt.Fprintf(os.Stderr, "! %v\n", w)
			}
		}
		// truncated is the machine-readable half of those warnings. This
		// command is meant for scripts and agents, and a valid envelope that
		// silently omits the one source holding a pending decision reads as
		// "nothing is waiting" — a failure that looks like an answer.
		if len(problems) > 0 {
			agg.Truncated = true
		}
		out, err := json.MarshalIndent(feed.Feed{
			Schema:    feed.Schema,
			Items:     agg.Items,
			Truncated: agg.Truncated,
		}, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "encode: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(out))
		return
	}

	if err := board.Run(built, *all); err != nil {
		fmt.Fprintf(os.Stderr, "inbox: %v\n", err)
		os.Exit(1)
	}
}
