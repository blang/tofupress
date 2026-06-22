package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// BuildVersion is injected at compile time via ldflags.
var BuildVersion string

// BuildCommit is injected at compile time via ldflags.
var BuildCommit string

// BuildTime is injected at compile time via ldflags.
var BuildTime string

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("tofupress %s\n", BuildVersion)
		if BuildCommit != "" {
			fmt.Printf("  commit: %s\n", BuildCommit)
		}
		if BuildTime != "" {
			fmt.Printf("  built:  %s\n", BuildTime)
		}
	},
}
