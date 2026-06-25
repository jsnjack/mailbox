package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jsnjack/mailbox/internal/ai"
	"github.com/jsnjack/mailbox/internal/config"
	"github.com/spf13/cobra"
	"github.com/zalando/go-keyring"
)

var aiKeyCmd = &cobra.Command{
	Use:   "set-ai-key",
	Short: "Store the AI provider API key in the OS keyring (reads the key from stdin)",
	Long: "Reads an API key from standard input and stores it in the OS keyring under " +
		"service \"mailbox-ai\", keyed by the provider from the config file. Pipe the key " +
		"to avoid it appearing on screen, e.g.: printf '%s' \"$KEY\" | mailbox set-ai-key",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runSetAIKey,
}

func init() {
	rootCmd.AddCommand(aiKeyCmd)
}

func runSetAIKey(cmd *cobra.Command, args []string) error {
	cfgPath, err := config.ConfigFilePath()
	if err != nil {
		return err
	}
	cfg, err := ai.LoadConfig(cfgPath)
	if err != nil {
		return err
	}
	if cfg.Provider == "" {
		return fmt.Errorf("set [ai].provider in %s before storing a key", cfgPath)
	}

	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read key from stdin: %w", err)
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return fmt.Errorf("no key provided on stdin")
	}

	if err := keyring.Set(aiKeyringService, cfg.Provider, key); err != nil {
		return fmt.Errorf("store key in keyring: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Stored AI key for provider %q in keyring service %q.\n", cfg.Provider, aiKeyringService)
	return nil
}
