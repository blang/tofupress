//go:build integration

//nolint:gosec // integration tests execute test binary, oras, and access network
package tofupress

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAcceptance_OCI_CompliantBundle(t *testing.T) {
	bin := buildBinary(t)
	repo := createLocalGitModuleRepo(t, "oci-dep", map[string]string{
		"main.tf": `output "remote" { value = "ok" }`,
	})
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", fmt.Sprintf(`module "dep" { source = %q }`, gitFileSource(repo)))

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--oci-compliant")

	metadata := metadataFromArtifact(t, artifact)
	assert.True(t, metadata.Command.Options.OCICompliant)

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}

func TestAcceptance_OCI_StructureVsNonOCI(t *testing.T) {
	bin := buildBinary(t)
	repo := createLocalGitModuleRepo(t, "oci-struct", map[string]string{
		"main.tf": `output "struct" { value = "test" }`,
	})
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", fmt.Sprintf(`module "d" { source = %q }`, gitFileSource(repo)))

	// OCI-compliant
	ociArtifact := filepath.Join(t.TempDir(), "oci.zip")
	runTofuPressBundle(t, bin, src, ociArtifact, "--oci-compliant")
	validateArchiveWithAllTools(t, serveArtifact(t, ociArtifact))

	// Non-OCI
	nonOciArtifact := filepath.Join(t.TempDir(), "non-oci.zip")
	runTofuPressBundle(t, bin, src, nonOciArtifact, "--format=zip")
	validateArchiveWithAllTools(t, serveArtifact(t, nonOciArtifact))

	// OCI-compliant must NOT contain legacy sourcetree/
	cmd := exec.Command("unzip", "-l", ociArtifact)
	out, _ := cmd.CombinedOutput()
	assert.NotContains(t, string(out), "sourcetree/", "OCI-compliant must not contain legacy sourcetree/")
}

func TestAcceptance_OCI_PushPull(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping OCI push/pull test in short mode")
	}
	requireOciCredentials(t)

	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `output "oci" { value = "push-pull-test" }`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--oci-compliant")

	tag := fmt.Sprintf("acceptance-%d-pushpull", time.Now().Unix())
	image := "docker.io/blang/tofupress-test"

	t.Cleanup(func() {
		ref := fmt.Sprintf("%s:%s", image, tag)
		cmd := exec.Command("oras", "manifest", "delete", ref)
		cmd.Env = append(os.Environ(), ociEnv()...)
		_ = cmd.Run() // best-effort cleanup
	})

	ociPush(t, image, tag, artifact)

	consumerDir := t.TempDir()
	writeConsumerModule(t, consumerDir, fmt.Sprintf("oci://%s?tag=%s", image, tag))

	tool := requireIACTool(t, "tofu")
	runIACTool(t, tool, consumerDir, "init", "-backend=false", "-input=false", "-no-color")
	runIACTool(t, tool, consumerDir, "validate", "-no-color")
}

func TestAcceptance_OCI_TerraformInit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping OCI terraform init test in short mode")
	}
	requireOciCredentials(t)

	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `output "oci_tf" { value = "terraform-init" }`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--oci-compliant")

	tag := fmt.Sprintf("acceptance-%d-tfinit", time.Now().Unix())
	image := "docker.io/blang/tofupress-test"

	t.Cleanup(func() {
		ref := fmt.Sprintf("%s:%s", image, tag)
		cmd := exec.Command("oras", "manifest", "delete", ref)
		cmd.Env = append(os.Environ(), ociEnv()...)
		_ = cmd.Run()
	})

	ociPush(t, image, tag, artifact)

	consumerDir := t.TempDir()
	writeConsumerModule(t, consumerDir, fmt.Sprintf("oci://%s?tag=%s", image, tag))

	tool := requireIACTool(t, "terraform")
	runIACTool(t, tool, consumerDir, "init", "-backend=false", "-input=false", "-no-color")
	runIACTool(t, tool, consumerDir, "validate", "-no-color")
}

func TestAcceptance_OCI_StripConfigOnly(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `output "oci_stripped" { value = "config-only" }`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--oci-compliant", "--strip=config-only")

	metadata := metadataFromArtifact(t, artifact)
	assert.True(t, metadata.Command.Options.OCICompliant)
	assert.Equal(t, string(StripModeConfigOnly), metadata.Command.Options.StripMode)

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}
