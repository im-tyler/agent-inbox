package main

import (
	"flag"
	"strings"
)

// parseInterspersed accepts options before or after positional arguments,
// which the standard FlagSet parser does not: it stops at the first
// positional, so documented trailing flags ("send NAME MSG --timeout 1s")
// used to become prompt text. A literal -- ends option recognition.
func parseInterspersed(fs *flag.FlagSet, argv []string) error {
	var options, positional []string
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			positional = append(positional, argv[i+1:]...)
			break
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		name := strings.TrimPrefix(arg, "-")
		name = strings.TrimPrefix(name, "-")
		name, _, hasValue := strings.Cut(name, "=")
		f := fs.Lookup(name)
		if f == nil {
			// Preserve FlagSet's help and unknown-option behavior.
			return fs.Parse([]string{arg})
		}
		options = append(options, arg)
		if hasValue {
			continue
		}
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			continue
		}
		if i+1 == len(argv) {
			// Let FlagSet report its ordinary missing-value error.
			return fs.Parse(options)
		}
		i++
		options = append(options, argv[i])
	}
	options = append(options, "--")
	return fs.Parse(append(options, positional...))
}
