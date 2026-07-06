package cmd

import (
	"encoding/json"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/blang/tofupress/pkg/tofupress"
)

// BuildVersion is injected at compile time via ldflags (`just build`/`just
// build-all`). A plain `go build` leaves it empty; EffectiveBuildInfo fills the
// gap from runtime/debug.ReadBuildInfo so every binary carries provenance.
var BuildVersion string

// BuildCommit is injected at compile time via ldflags.
var BuildCommit string

// BuildTime is injected at compile time via ldflags.
var BuildTime string

// EffectiveBuildInfo returns the provenance to surface for this binary.
//
// ldflags-injected values win when present (versioned `just build`). For a
// plain `go build` — where the ldflags are empty — it synthesizes provenance
// from runtime/debug.ReadBuildInfo (VCS revision + time, or a `dev-<timestamp>`
// tag) so meta.json is never blank (review item 7 / QA-6).
func EffectiveBuildInfo() tofupress.BuildInfo {
	if BuildVersion != "" || BuildCommit != "" || BuildTime != "" {
		return tofupress.BuildInfo{Version: BuildVersion, Commit: BuildCommit, Time: BuildTime}
	}
	return synthesizedBuildInfo()
}

// synthesizedBuildInfo derives provenance from the embedded build info for
// unversioned `go build` binaries.
func synthesizedBuildInfo() tofupress.BuildInfo {
	commit, built, vcsModified, moduleVersion := readVCSBuildInfo()

	commit = normalizeCommit(commit, vcsModified)
	if commit == "" {
		commit = "unknown"
	}
	if built == "" {
		built = time.Now().UTC().Format(time.RFC3339)
	}
	version := moduleVersion
	if version == "" {
		version = synthesizeDevVersion(built)
	}
	return tofupress.BuildInfo{Version: version, Commit: commit, Time: built}
}

// readVCSBuildInfo extracts VCS revision/time/modified and the module version
// from the embedded build info. Returns empty strings when absent.
func readVCSBuildInfo() (commit, built string, modified bool, moduleVersion string) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			commit = s.Value
		case "vcs.time":
			built = s.Value
		case "vcs.modified":
			modified = strings.EqualFold(s.Value, "true")
		}
	}
	// Module version is "(devel)" for local builds; a real semver only for
	// `go install`/`go build` of a tagged release.
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		moduleVersion = info.Main.Version
	}
	return
}

// normalizeCommit truncates the full revision to a short hash and appends a
// "-dirty" suffix when the working tree was modified at build time.
func normalizeCommit(commit string, vcsModified bool) string {
	if commit != "" && len(commit) > 12 {
		commit = commit[:12]
	}
	if vcsModified && commit != "" {
		commit += "-dirty"
	}
	return commit
}

// synthesizeDevVersion builds a distinguishable `dev-<timestamp>` tag so two
// plain `go build` runs never share identical-but-blank provenance. Prefers
// the VCS time; falls back to the wall clock when there is no VCS info.
func synthesizeDevVersion(built string) string {
	if built != "" {
		if t, err := time.Parse(time.RFC3339, built); err == nil {
			return "dev-" + t.UTC().Format("20060102150405")
		}
	}
	return "dev-" + time.Now().UTC().Format("20060102150405")
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		bi := EffectiveBuildInfo()
		jsonOut, _ := cmd.Flags().GetBool("json")
		if jsonOut {
			// Scriptable version check (review item 9): mirror the text fields as JSON.
			out := struct {
				Version   string `json:"version"`
				Commit    string `json:"commit"`
				BuildTime string `json:"build_time"`
			}{Version: bi.Version, Commit: bi.Commit, BuildTime: bi.Time}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			if err := enc.Encode(out); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), err) //nolint:errcheck // stderr write is best-effort
			}
			return
		}
		version := bi.Version
		if version == "" {
			version = "(development build; use 'just build' for versioned builds)"
		}
		fmt.Printf("tofupress %s\n", version)
		if bi.Commit != "" {
			fmt.Printf("  commit: %s\n", bi.Commit)
		}
		if bi.Time != "" {
			fmt.Printf("  built:  %s\n", bi.Time)
		}
	},
}

func init() {
	versionCmd.Flags().Bool("json", false, "Output version information as JSON (review item 9)")
}
