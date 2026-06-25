package main

import (
	"log/slog"
	"path/filepath"

	"github.com/jsnjack/mailbox/internal/config"
	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/spf13/cobra"
)

const traceLogPath = "/tmp/mailbox.log"

var (
	flagDebug       bool
	flagTrace       bool
	flagConfig      string
	flagCredentials string

	// logCleanup is set by PersistentPreRunE and invoked from main on exit.
	logCleanup = func() {}
)

// defaultCredentialsPath returns the conventional location for the Google OAuth
// client JSON, alongside the config file.
func defaultCredentialsPath() string {
	p, err := config.ConfigFilePath()
	if err != nil {
		return "credentials.json"
	}
	return filepath.Join(filepath.Dir(p), "credentials.json")
}

// credentialsPath returns the configured OAuth client JSON path.
func credentialsPath() string { return flagCredentials }

// rootCmd is the application entry command. Running it with no subcommand
// launches the GTK application.
var rootCmd = &cobra.Command{
	Use:           "mailbox",
	Short:         "A native, fast Gmail client",
	Version:       Version,
	SilenceUsage:  true,
	SilenceErrors: true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		level, tracePath := "", ""
		switch {
		case flagTrace:
			level, tracePath = "trace", traceLogPath
		case flagDebug:
			level = "debug"
		}
		logCleanup = logging.Init(tracePath, level)
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		slog.Info("mailbox starting", "version", Version)
		return launchUI()
	},
}

func init() {
	rootCmd.SetVersionTemplate("{{.Version}}\n")
	flags := rootCmd.PersistentFlags()
	flags.BoolVarP(&flagDebug, "debug", "d", false,
		"Debug-level logging on stderr.")
	flags.BoolVar(&flagTrace, "trace", false,
		"Trace-level logs to "+traceLogPath+" (truncated each run).")
	flags.StringVarP(&flagConfig, "config", "c", "",
		"Path to the config file (default ~/.config/mailbox/config.toml).")
	flags.StringVar(&flagCredentials, "credentials", defaultCredentialsPath(),
		"Path to the Google OAuth client JSON.")
}
