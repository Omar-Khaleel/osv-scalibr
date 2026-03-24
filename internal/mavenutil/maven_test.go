// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mavenutil

import (
	"path/filepath"
	"os"
	"testing"

	"deps.dev/util/resolve"
	"deps.dev/util/semver"
	"github.com/google/osv-scalibr/testing/extracttest"
	"github.com/google/osv-scalibr/extractor/filesystem"
	"github.com/google/osv-scalibr/fs"
)

func TestParentPOMPath(t *testing.T) {
	input := extracttest.GenerateScanInputMock(t, extracttest.ScanInputMockConfig{
		Path: filepath.Join("testdata", "my-app", "pom.xml"),
	})
	defer extracttest.CloseTestScanInput(t, input)

	tests := []struct {
		currentPath, relativePath, rootPath string
		want                      string
	}{
		// testdata
		// |- maven
		// |  |- my-app
		// |  |  |- pom.xml
		// |  |- parent
		// |  |  |- pom.xml
		// |- pom.xml
		{
			// Parent path is specified correctly.
			currentPath:  filepath.Join("testdata", "my-app", "pom.xml"),
			relativePath: "../parent/pom.xml",
			rootPath:     "testdata",
			want:         filepath.Join("testdata", "parent", "pom.xml"),
		},
		{
			// Wrong file name is specified in relative path.
			currentPath:  filepath.Join("testdata", "my-app", "pom.xml"),
			relativePath: "../parent/abc.xml",
			rootPath:     "testdata",
			want:         "",
		},
		{
			// Wrong directory is specified in relative path.
			currentPath:  filepath.Join("testdata", "my-app", "pom.xml"),
			relativePath: "../not-found/pom.xml",
			rootPath:     "testdata",
			want:         "",
		},
		{
			// Only directory is specified.
			currentPath:  filepath.Join("testdata", "my-app", "pom.xml"),
			relativePath: "../parent",
			rootPath:     "testdata",
			want:         filepath.Join("testdata", "parent", "pom.xml"),
		},
		{
			// Parent relative path is default to '../pom.xml'.
			currentPath:  filepath.Join("testdata", "my-app", "pom.xml"),
			relativePath: "",
			rootPath:     "testdata",
			want:         filepath.Join("testdata", "pom.xml"),
		},
		{
			// No pom.xml is found even in the default path.
			currentPath:  filepath.Join("testdata", "pom.xml"),
			relativePath: "",
			rootPath:     "testdata",
			want:         "",
		},
	}
	for _, tt := range tests {
		got := ParentPOMPath(&input, tt.currentPath, tt.relativePath, tt.rootPath)
		if got != filepath.ToSlash(tt.want) {
			t.Errorf("ParentPOMPath(%s, %s): got %s, want %s", tt.currentPath, tt.relativePath, got, tt.want)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	versionKey := func(name string, version string) resolve.VersionKey {
		return resolve.VersionKey{
			PackageKey: resolve.PackageKey{
				System: resolve.Maven,
				Name:   name,
			},
			Version: version,
		}
	}
	semVer := func(version string) *semver.Version {
		parsed, _ := resolve.Maven.Semver().Parse(version)
		return parsed
	}

	tests := []struct {
		vk   resolve.VersionKey
		a, b *semver.Version
		want int
	}{
		{
			versionKey("abc:xyz", "1.0.0"),
			semVer("1.2.3"),
			semVer("1.2.3"),
			0,
		},
		{
			versionKey("abc:xyz", "1.0.0"),
			semVer("1.2.3"),
			semVer("2.3.4"),
			-1,
		},
		{
			versionKey("com.google.guava:guava", "1.0.0"),
			semVer("1.2.3"),
			semVer("2.3.4"),
			-1,
		},
		{
			versionKey("com.google.guava:guava", "1.0.0"),
			semVer("1.2.3-jre"),
			semVer("2.3.4-jre"),
			-1,
		},
		{
			versionKey("com.google.guava:guava", "1.0.0"),
			semVer("1.2.3-android"),
			semVer("2.3.4-android"),
			-1,
		},
		{
			versionKey("com.google.guava:guava", "1.0.0"),
			semVer("2.3.4-android"),
			semVer("1.2.3-jre"),
			-1,
		},
		{
			versionKey("com.google.guava:guava", "1.0.0-jre"),
			semVer("1.2.3-android"),
			semVer("1.2.3-jre"),
			-1,
		},
		{
			versionKey("com.google.guava:guava", "1.0.0-android"),
			semVer("1.2.3-android"),
			semVer("1.2.3-jre"),
			1,
		},
		{
			versionKey("commons-io:commons-io", "1.0.0"),
			semVer("1.2.3"),
			semVer("2.3.4"),
			-1,
		},
		{
			versionKey("commons-io:commons-io", "1.0.0"),
			semVer("1.2.3"),
			semVer("20010101.000000"),
			1,
		},
	}
	for _, tt := range tests {
		got := CompareVersions(tt.vk, tt.a, tt.b)
		if got != tt.want {
			t.Errorf("CompareVersions(%v, %v, %v): got %b, want %b", tt.vk, tt.a, tt.b, got, tt.want)
		}
	}
}

func TestIsWithinRoot(t *testing.T) {
	t.Parallel()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get current working directory: %v", err)
	}

	tests := []struct {
		name       string
		rootPath   string
		targetPath string
		want       bool
	}{
		{
			name:       "target is inside root",
			rootPath:   filepath.Join(cwd, "testdata"),
			targetPath: filepath.Join(cwd, "testdata", "child"),
			want:       true,
		},
		{
			name:       "target is the root",
			rootPath:   filepath.Join(cwd, "testdata"),
			targetPath: filepath.Join(cwd, "testdata"),
			want:       true,
		},
		{
			name:       "target escapes root via relative path",
			rootPath:   filepath.Join(cwd, "testdata", "child"),
			targetPath: filepath.Join(cwd, "testdata", "child", "..", "sibling"),
			want:       false,
		},
		{
			name:       "target escapes root completely",
			rootPath:   filepath.Join(cwd, "testdata"),
			targetPath: filepath.Join(cwd, ".."),
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsWithinRoot(tt.rootPath, tt.targetPath); got != tt.want {
				t.Errorf("IsWithinRoot() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParentPOMPath_Containment(t *testing.T) {
	t.Parallel()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get current working directory: %v", err)
	}

	input := filesystem.ScanInput{FS: fs.DirFS(".")}
	rootPath := filepath.Join(cwd, "testdata")
	currentPath := filepath.Join(rootPath, "child", "pom.xml")

	tests := []struct {
		name         string
		relativePath string
		want         string
	}{
		{
			name:         "parent directory outside root",
			relativePath: "../../pom.xml",
			want:         "", // Should be blocked
		},
		{
			name:         "absolute path outside root",
			relativePath: "/etc/passwd",
			want:         "", // Should be blocked
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ParentPOMPath(&input, currentPath, tt.relativePath, rootPath); got != tt.want {
				t.Errorf("ParentPOMPath() = %v, want %v", got, tt.want)
			}
		})
	}
}
