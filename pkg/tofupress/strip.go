package tofupress

import (
	"fmt"
	"strings"
)

// StripMode controls how much non-generated package content is retained.
type StripMode string

// Strip mode constants define the supported ways to filter bundle content.
const (
	StripModeNone       StripMode = "none"
	StripModeModuleDir  StripMode = "module-dir"
	StripModeConfigOnly StripMode = "config-only"
)

// ParseStripMode parses CLI/API strip mode input. Empty input means the safe default.
func ParseStripMode(raw string) (StripMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(StripModeModuleDir):
		return StripModeModuleDir, nil
	case string(StripModeNone):
		return StripModeNone, nil
	case string(StripModeConfigOnly), "tf-only":
		return StripModeConfigOnly, nil
	default:
		return "", fmt.Errorf("unsupported strip mode %q (use none, module-dir, or config-only)", raw)
	}
}

// IsAggressive reports whether this mode may remove files despite possible runtime reads.
func (m StripMode) IsAggressive() bool {
	return m == StripModeConfigOnly
}
