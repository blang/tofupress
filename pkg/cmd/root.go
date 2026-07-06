// Package cmd implements the tofupress CLI commands.
package cmd

import (
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/golang-cz/devslog"
	"github.com/spf13/cobra"
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use: "tofupress",
	Version: func() string {
		if v := EffectiveBuildInfo().Version; v != "" {
			return v
		}
		return "(development build)"
	}(),
	Short: "TofuPress - OpenTofu/Terraform module bundler",
	Long: `A CLI that takes an OpenTofu/Terraform root module, recursively resolves
all referenced modules, and bundles everything into a single self-contained
artifact (tar.gz or OCI Terraform module) — no external module sources left
to resolve at runtime.`,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		flagDebug, _ := cmd.Flags().GetBool("debug")
		logLevel := slog.LevelInfo
		if flagDebug {
			logLevel = slog.LevelDebug
		}
		setupLogger(logLevel)
	},
}

// Execute runs the root command.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func setupLogger(lvl slog.Level) {
	slogOpts := &slog.HandlerOptions{
		AddSource: false,
		Level:     lvl,
	}

	opts := &devslog.Options{
		HandlerOptions:    slogOpts,
		MaxSlicePrintSize: 100,
		SortKeys:          true,
		TimeFormat:        "[15:04:05]",
		NewLineAfterLog:   false,
		DebugColor:        devslog.Magenta,
		StringerFormatter: true,
	}

	logger := slog.New(devslog.NewHandler(os.Stderr, opts))
	slog.SetDefault(logger)
}

func init() {
	rootCmd.PersistentFlags().BoolP("debug", "", false, "Enable verbose logging")

	// Register subcommands
	rootCmd.AddCommand(resolveCmd)
	rootCmd.AddCommand(bundleCmd)
	rootCmd.AddCommand(metadataCmd)
	rootCmd.AddCommand(versionCmd)
}

// installSignalCleanup ensures cleanup runs on SIGINT/SIGTERM. Go's defers
// do NOT fire on os.Exit, and a normal function return does not happen when a
// signal kills the process — verified in the review session: a Ctrl-C during a
// slow bundle leaves an orphaned /tmp/tofupress-* dir behind. This installs a
// one-shot signal handler that invokes cleanup and exits. It returns a stop
// function that restores the previous signal handling (so a successful return
// path is never interrupted by the handler). The cleanup is guarded by a
// sync.Once so it never double-fires with the deferred cleanup on the happy
// path (review item 9).
func installSignalCleanup(cleanup func()) (stop func()) {
	var once sync.Once
	wrapped := func() { once.Do(cleanup) }
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		_, ok := <-ch
		if !ok {
			return
		}
		wrapped()
		os.Exit(130) // 128+SIGINT, the conventional shell exit-on-Ctrl-C
	}()
	return func() {
		signal.Stop(ch)
		close(ch)
		wrapped() // belt-and-suspenders: happy-path cleanup also runs via defer
	}
}
