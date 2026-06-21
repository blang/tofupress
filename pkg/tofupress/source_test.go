//nolint:govet,gosec // test structs prioritize readability over memory layout; test paths are not credentials
package tofupress

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifySource_Local(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		pwd  string
		want ModuleSource
	}{
		{
			name: "relative current dir",
			raw:  "./modules/mod1",
			pwd:  "/root",
			want: ModuleSource{
				Raw:  "./modules/mod1",
				Type: SourceLocal,
			},
		},
		{
			name: "relative parent dir",
			raw:  "../shared",
			pwd:  "/root/sub",
			want: ModuleSource{
				Raw:  "../shared",
				Type: SourceLocal,
			},
		},
		{
			name: "deep relative path",
			raw:  "../../common/modules/vpc",
			pwd:  "/root/a/b/c", //nolint:gosec // test path, not credential
			want: ModuleSource{
				Raw:  "../../common/modules/vpc",
				Type: SourceLocal,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifySource(tt.raw, tt.pwd)
			assert.Equal(t, tt.want.Type, got.Type)
			assert.Equal(t, tt.want.Raw, got.Raw)
		})
	}
}

func TestClassifySource_Git(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		pwd  string
		want ModuleSource
	}{
		{
			name: "git https basic",
			raw:  "git::https://github.com/user/repo.git",
			pwd:  "",
			want: ModuleSource{
				Raw:         "git::https://github.com/user/repo.git",
				Type:        SourceGit,
				PackageAddr: "git::https://github.com/user/repo.git",
			},
		},
		{
			name: "git with ref",
			raw:  "git::https://github.com/user/repo.git?ref=v1.0",
			pwd:  "",
			want: ModuleSource{
				Raw:         "git::https://github.com/user/repo.git?ref=v1.0",
				Type:        SourceGit,
				PackageAddr: "git::https://github.com/user/repo.git?ref=v1.0",
				Ref:         "v1.0",
			},
		},
		{
			name: "git with subdir",
			raw:  "git::https://github.com/user/repo.git//modules/vpc?ref=v1.0",
			pwd:  "",
			want: ModuleSource{
				Raw:         "git::https://github.com/user/repo.git//modules/vpc?ref=v1.0",
				Type:        SourceGit,
				PackageAddr: "git::https://github.com/user/repo.git?ref=v1.0",
				SubDir:      "modules/vpc",
				Ref:         "v1.0",
			},
		},
		{
			name: "git with branch ref",
			raw:  "git::https://github.com/blang/tf-test2.git?ref=master",
			pwd:  "",
			want: ModuleSource{
				Raw:         "git::https://github.com/blang/tf-test2.git?ref=master",
				Type:        SourceGit,
				PackageAddr: "git::https://github.com/blang/tf-test2.git?ref=master",
				Ref:         "master",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifySource(tt.raw, tt.pwd)
			assert.Equal(t, tt.want.Type, got.Type)
			assert.Equal(t, tt.want.PackageAddr, got.PackageAddr)
			assert.Equal(t, tt.want.SubDir, got.SubDir)
			assert.Equal(t, tt.want.Ref, got.Ref)
		})
	}
}

func TestClassifySource_HTTP(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		pwd  string
		want ModuleSource
	}{
		{
			name: "http archive",
			raw:  "https://example.com/module.tar.gz",
			pwd:  "",
			want: ModuleSource{
				Raw:         "https://example.com/module.tar.gz",
				Type:        SourceHTTP,
				PackageAddr: "https://example.com/module.tar.gz",
			},
		},
		{
			name: "http with subdir",
			raw:  "https://example.com/archive.tar.gz//subdir",
			pwd:  "",
			want: ModuleSource{
				Raw:         "https://example.com/archive.tar.gz//subdir",
				Type:        SourceHTTP,
				PackageAddr: "https://example.com/archive.tar.gz",
				SubDir:      "subdir",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifySource(tt.raw, tt.pwd)
			assert.Equal(t, tt.want.Type, got.Type)
			assert.Equal(t, tt.want.PackageAddr, got.PackageAddr)
			assert.Equal(t, tt.want.SubDir, got.SubDir)
		})
	}
}

func TestClassifySource_Registry(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		pwd  string
		want ModuleSource
	}{
		{
			name: "registry basic",
			raw:  "hashicorp/consul/aws",
			pwd:  "",
			want: ModuleSource{
				Raw:  "hashicorp/consul/aws",
				Type: SourceRegistry,
			},
		},
		{
			name: "registry with version",
			raw:  "hashicorp/consul/aws?version=1.0.0",
			pwd:  "",
			want: ModuleSource{
				Raw:  "hashicorp/consul/aws?version=1.0.0",
				Type: SourceRegistry,
				Ref:  "1.0.0",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifySource(tt.raw, tt.pwd)
			assert.Equal(t, tt.want.Type, got.Type)
			assert.Equal(t, tt.want.Ref, got.Ref)
		})
	}
}

func TestClassifySource_S3(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		pwd  string
		want ModuleSource
	}{
		{
			name: "s3 basic",
			raw:  "s3::https://bucket.s3.amazonaws.com/module.zip",
			pwd:  "",
			want: ModuleSource{
				Raw:         "s3::https://bucket.s3.amazonaws.com/module.zip",
				Type:        SourceS3,
				PackageAddr: "s3::https://bucket.s3.amazonaws.com/module.zip",
			},
		},
		{
			name: "s3 with subdir",
			raw:  "s3::https://bucket.s3.amazonaws.com/module.zip//subdir",
			pwd:  "",
			want: ModuleSource{
				Raw:         "s3::https://bucket.s3.amazonaws.com/module.zip//subdir",
				Type:        SourceS3,
				PackageAddr: "s3::https://bucket.s3.amazonaws.com/module.zip",
				SubDir:      "subdir",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifySource(tt.raw, tt.pwd)
			assert.Equal(t, tt.want.Type, got.Type)
			assert.Equal(t, tt.want.PackageAddr, got.PackageAddr)
			assert.Equal(t, tt.want.SubDir, got.SubDir)
		})
	}
}

func TestClassifySource_GCS(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		pwd  string
		want ModuleSource
	}{
		{
			name: "gcs basic",
			raw:  "gcs::https://www.googleapis.com/storage/v1/bucket/module.tar.gz",
			pwd:  "",
			want: ModuleSource{
				Raw:         "gcs::https://www.googleapis.com/storage/v1/bucket/module.tar.gz",
				Type:        SourceGCS,
				PackageAddr: "gcs::https://www.googleapis.com/storage/v1/bucket/module.tar.gz",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifySource(tt.raw, tt.pwd)
			assert.Equal(t, tt.want.Type, got.Type)
			assert.Equal(t, tt.want.PackageAddr, got.PackageAddr)
		})
	}
}

func TestClassifySource_OCI(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		pwd  string
		want ModuleSource
	}{
		{
			name: "oci basic",
			raw:  "oci://registry.example.com/modules/my-module",
			pwd:  "",
			want: ModuleSource{
				Raw:         "oci://registry.example.com/modules/my-module",
				Type:        SourceOCI,
				PackageAddr: "oci://registry.example.com/modules/my-module",
			},
		},
		{
			name: "oci with tag",
			raw:  "oci://registry.example.com/modules/my-module?tag=v1.0",
			pwd:  "",
			want: ModuleSource{
				Raw:         "oci://registry.example.com/modules/my-module?tag=v1.0",
				Type:        SourceOCI,
				PackageAddr: "oci://registry.example.com/modules/my-module?tag=v1.0",
				Ref:         "v1.0",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifySource(tt.raw, tt.pwd)
			assert.Equal(t, tt.want.Type, got.Type)
			assert.Equal(t, tt.want.PackageAddr, got.PackageAddr)
			assert.Equal(t, tt.want.Ref, got.Ref)
		})
	}
}

func TestSplitPackageSubdir(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantPkg string
		wantSub string
	}{
		{
			name:    "git no subdir",
			input:   "git::https://github.com/user/repo.git",
			wantPkg: "git::https://github.com/user/repo.git",
			wantSub: "",
		},
		{
			name:    "git with subdir",
			input:   "git::https://github.com/user/repo.git//modules/vpc",
			wantPkg: "git::https://github.com/user/repo.git",
			wantSub: "modules/vpc",
		},
		{
			name:    "git with subdir and ref",
			input:   "git::https://github.com/user/repo.git//modules/vpc?ref=v1",
			wantPkg: "git::https://github.com/user/repo.git?ref=v1",
			wantSub: "modules/vpc",
		},
		{
			name:    "http with subdir",
			input:   "https://example.com/archive.tar.gz//subdir",
			wantPkg: "https://example.com/archive.tar.gz",
			wantSub: "subdir",
		},
		{
			name:    "http with subdir and query",
			input:   "https://example.com/archive.tar.gz//subdir?version=1.0",
			wantPkg: "https://example.com/archive.tar.gz?version=1.0",
			wantSub: "subdir",
		},
		{
			name:    "s3 with subdir",
			input:   "s3::https://bucket.s3.amazonaws.com/module.zip//modules/vpc",
			wantPkg: "s3::https://bucket.s3.amazonaws.com/module.zip",
			wantSub: "modules/vpc",
		},
		{
			name:    "gcs with subdir",
			input:   "gcs::https://storage.googleapis.com/bucket/module.tar.gz//subdir",
			wantPkg: "gcs::https://storage.googleapis.com/bucket/module.tar.gz",
			wantSub: "subdir",
		},
		{
			name:    "deep subdir path",
			input:   "git::https://github.com/user/repo.git//modules/vpc/submodules/public?ref=v1.0",
			wantPkg: "git::https://github.com/user/repo.git?ref=v1.0",
			wantSub: "modules/vpc/submodules/public",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPkg, gotSub := SplitPackageSubdir(tt.input)
			assert.Equal(t, tt.wantPkg, gotPkg)
			assert.Equal(t, tt.wantSub, gotSub)
		})
	}
}

func TestIsLocalSource(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{"current dir", "./modules/mod1", true},
		{"parent dir", "../shared", true},
		{"deep parent", "../../common", true},
		{"git source", "git::https://github.com/user/repo.git", false},
		{"http source", "https://example.com/module.tar.gz", false},
		{"registry", "hashicorp/consul/aws", false},
		{"absolute path", "/absolute/path", false},
		{"no prefix", "modules/mod1", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsLocalSource(tt.raw)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIsRegistrySource(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{"registry basic", "hashicorp/consul/aws", true},
		{"registry with version", "hashicorp/consul/aws?version=1.0.0", true},
		{"local path", "./modules/mod1", false},
		{"git source", "git::https://github.com/user/repo.git", false},
		{"http source", "https://example.com/module.tar.gz", false},
		{"two parts", "hashicorp/consul", false},
		{"four parts not registry", "hashicorp/consul/aws/extra", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRegistrySource(tt.raw)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestExtractRef(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"no ref", "https://github.com/user/repo.git", ""},
		{"git ref", "https://github.com/user/repo.git?ref=v1.0", "v1.0"},
		{"git ref branch", "https://github.com/user/repo.git?ref=master", "master"},
		{"version", "hashicorp/consul/aws?version=1.0.0", "1.0.0"},
		{"oci tag", "oci://registry/module?tag=v1.0", "v1.0"},
		{"multiple params", "https://github.com/user/repo.git?ref=v1.0&depth=1", "v1.0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractRef(tt.raw)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSourceType_String(t *testing.T) {
	tests := []struct {
		sourceType SourceType
		want       string
	}{
		{SourceLocal, "local"},
		{SourceGit, "git"},
		{SourceHTTP, "http"},
		{SourceRegistry, "registry"},
		{SourceS3, "s3"},
		{SourceGCS, "gcs"},
		{SourceOCI, "oci"},
		{SourceType(-1), "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := tt.sourceType.String()
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestClassifySource_Integration(t *testing.T) {
	// Test real-world examples from tf-test1 and tf-test2
	tests := []struct {
		name     string
		raw      string
		wantType SourceType
	}{
		{
			name:     "tf-test1 local module",
			raw:      "./modules/mod1",
			wantType: SourceLocal,
		},
		{
			name:     "tf-test1 git master",
			raw:      "git::https://github.com/blang/tf-test2//modules/mod1?ref=master",
			wantType: SourceGit,
		},
		{
			name:     "tf-test1 git v1.0.0",
			raw:      "git::https://github.com/blang/tf-test2//modules/mod2?ref=v1.0.0",
			wantType: SourceGit,
		},
		{
			name:     "relative parent",
			raw:      "../ext",
			wantType: SourceLocal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifySource(tt.raw, "/root")
			require.Equal(t, tt.wantType, got.Type, "source type mismatch")
		})
	}
}
