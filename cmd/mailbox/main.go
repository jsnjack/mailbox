package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	// As the system mailto handler the app is invoked as `mailbox mailto:…`.
	// Pull that URI out and hide it from cobra (which would reject an unknown
	// positional, since the root command has subcommands), then run normally.
	// Non-nil even when empty: cobra.SetArgs(nil) is special-cased to fall back to
	// os.Args[1:], which would re-expose the stripped mailto: URI as an "unknown
	// command". An empty (non-nil) slice means "no args" → launch the GUI.
	rest := []string{}
	for _, a := range os.Args[1:] {
		if launchMailto == "" && strings.HasPrefix(strings.ToLower(a), "mailto:") {
			launchMailto = a
			continue
		}
		rest = append(rest, a)
	}
	rootCmd.SetArgs(rest)

	err := rootCmd.Execute()
	logCleanup()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
