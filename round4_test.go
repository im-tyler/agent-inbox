package main

import (
	"bytes"
	"flag"
	"testing"

)

// F04: a removed project is a reported change, not a silent disappearance.
func TestDiffFleetReportsRemoval(t *testing.T) {
	env := newFleetDiffInbox(t, "alpha")
	defer env.Close()
	base := map[string]fleetPin{"alpha": {Status: "idle"}}
	changes := diffFleet(base, map[string]fleetPin{}, env, nil)
	if len(changes) != 1 || changes[0].Project != "alpha" || changes[0].To != "removed" {
		t.Fatalf("removal not reported: %+v", changes)
	}
	// A non-matching watch suppresses it, matching the change/added behaviour.
	if changes := diffFleet(base, map[string]fleetPin{}, env, map[string]bool{"bravo": true}); len(changes) != 0 {
		t.Fatalf("watch filter ignored for removals: %+v", changes)
	}
}

// F03: documented trailing flags parse instead of becoming prompt text.
func TestParseInterspersed(t *testing.T) {
	run := func(argv ...string) (timeout string, asJSON bool, args []string) {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		var buf bytes.Buffer
		fs.SetOutput(&buf)
		d := fs.Duration("timeout", 0, "")
		j := fs.Bool("json", false, "")
		if err := parseInterspersed(fs, argv); err != nil {
			t.Fatalf("parse %v: %v (%s)", argv, err, buf.String())
		}
		return d.String(), *j, fs.Args()
	}
	to, js, args := run("alpha", "hello", "--timeout", "1s", "--json")
	if to != "1s" || !js || len(args) != 2 || args[0] != "alpha" || args[1] != "hello" {
		t.Fatalf("trailing flags mishandled: timeout=%s json=%v args=%v", to, js, args)
	}
	to, js, args = run("--timeout=2s", "alpha", "hello")
	if to != "2s" || js || len(args) != 2 {
		t.Fatalf("leading equals form mishandled: %s %v %v", to, js, args)
	}
	_, _, args = run("alpha", "--", "--json", "literal")
	if len(args) != 3 || args[1] != "--json" {
		t.Fatalf("literal -- mishandled: %v", args)
	}
	_, _, args = run("alpha", "-")
	if len(args) != 2 || args[1] != "-" {
		t.Fatalf("stdin sentinel mishandled: %v", args)
	}
	fs := flag.NewFlagSet("t2", flag.ContinueOnError)
	if err := parseInterspersed(fs, []string{"x", "--nope"}); err == nil {
		t.Fatal("unknown option accepted")
	}
}
