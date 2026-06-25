package main

import (
	"fmt"
	"os"
)

func main() {
	err := rootCmd.Execute()
	logCleanup()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
