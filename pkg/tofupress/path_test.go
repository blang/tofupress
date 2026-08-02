package tofupress

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPortableArchivePath(t *testing.T) {
	valid, err := portableArchivePath(filepath.Join("namespace", "module", "main.tf"))
	require.NoError(t, err)
	assert.Equal(t, "namespace/module/main.tf", valid)

	for _, unsafe := range []string{"", ".", "..", "../escape", "/absolute", `..\escape`, `C:\escape`, "line\nbreak"} {
		t.Run(unsafe, func(t *testing.T) {
			_, err := portableArchivePath(unsafe)
			require.Error(t, err)
		})
	}
}

func TestJoinPathWithin(t *testing.T) {
	root := t.TempDir()

	valid, err := joinPathWithin(root, "namespace/package")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "namespace", "package"), valid)

	for _, unsafe := range []string{"", ".", "..", "../escape", "/absolute", `..\escape`, `C:\escape`} {
		t.Run(unsafe, func(t *testing.T) {
			_, err := joinPathWithin(root, unsafe)
			require.Error(t, err)
		})
	}
}
