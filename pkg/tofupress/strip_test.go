package tofupress

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseStripMode(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		want       StripMode
		aggressive bool
	}{
		{name: "empty defaults to module dir", raw: "", want: StripModeModuleDir},
		{name: "module dir", raw: "module-dir", want: StripModeModuleDir},
		{name: "none", raw: "none", want: StripModeNone},
		{name: "config only", raw: "config-only", want: StripModeConfigOnly, aggressive: true},
		{name: "tf only alias", raw: "tf-only", want: StripModeConfigOnly, aggressive: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseStripMode(tt.raw)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.aggressive, got.IsAggressive())
		})
	}
}

func TestParseStripModeRejectsUnknown(t *testing.T) {
	_, err := ParseStripMode("delete-everything")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported strip mode")
	assert.Contains(t, err.Error(), "none")
	assert.Contains(t, err.Error(), "module-dir")
	assert.Contains(t, err.Error(), "config-only")
}
