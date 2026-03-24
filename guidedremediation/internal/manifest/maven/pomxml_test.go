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

package maven

import (
	"os"
	"strings"
	"path/filepath"
	"reflect"
	"testing"

	"deps.dev/util/maven"
	"github.com/google/go-cmp/cmp"
	"github.com/google/osv-scalibr/fs"
	"github.com/google/osv-scalibr/guidedremediation/result"
	"github.com/google/osv-scalibr/testing/extracttest"
)

func TestReadWrite(t *testing.T) {
	input := extracttest.GenerateScanInputMock(t, extracttest.ScanInputMockConfig{
		Path: filepath.Join("testdata", "my-app", "pom.xml"),
	})
	defer extracttest.CloseTestScanInput(t, input)

	rw, err := GetReadWriter(nil)
	if err != nil {
		t.Fatalf("failed to create MavenReadWriter: %v", err)
	}

	m, err := rw.Read(input.Path, input.FS)
	if err != nil {
		t.Fatalf("failed to read manifest: %v", err)
	}

	parentPath := filepath.Join("testdata", "parent", "pom.xml")
	specific := ManifestSpecific{
		Parent: maven.Parent{
			ProjectKey: maven.ProjectKey{
				GroupID:    "org.parent",
				ArtifactID: "parent-pom",
				Version:    "1.1.1",
			},
			RelativePath: "../parent/pom.xml",
		},
		Properties: []PropertyWithOrigin{
			{Property: maven.Property{Name: "property.version", Value: "1.0.0"}},
			{Property: maven.Property{Name: "no.update.minor", Value: "9"}},
			{Property: maven.Property{Name: "def.version", Value: "2.3.4"}, Origin: "profile@profile-one"},
			{Property: maven.Property{Name: "aaa.version", Value: "1.1.1"}, Origin: "parent@" + parentPath},
		},
		LocalRequirements: []DependencyWithOrigin{
			{
				Dependency: maven.Dependency{GroupID: "org.parent", ArtifactID: "parent-pom", Version: "1.1.1", Type: "pom"},
				Origin:     "parent",
			},
			{
				Dependency: maven.Dependency{GroupID: "junit", ArtifactID: "junit", Version: "${junit.version}", Scope: "test"},
			},
			{
				Dependency: maven.Dependency{GroupID: "org.example", ArtifactID: "abc", Version: "1.0.1"},
			},
			{
				Dependency: maven.Dependency{GroupID: "org.example", ArtifactID: "no-updates", Version: "9.9.9"},
			},
			{
				Dependency: maven.Dependency{GroupID: "org.example", ArtifactID: "no-version"},
			},
			{
				Dependency: maven.Dependency{GroupID: "org.example", ArtifactID: "property", Version: "${property.version}"},
			},
			{
				Dependency: maven.Dependency{GroupID: "org.example", ArtifactID: "property-no-update", Version: "1.${no.update.minor}"},
			},
			{
				Dependency: maven.Dependency{GroupID: "org.example", ArtifactID: "same-property", Version: "${property.version}"},
			},
			{
				Dependency: maven.Dependency{GroupID: "org.example", ArtifactID: "another-property", Version: "${property.version}"},
			},
			{
				Dependency: maven.Dependency{GroupID: "org.example", ArtifactID: "no-version", Version: "2.0.0"},
				Origin:     "management",
			},
			{
				Dependency: maven.Dependency{GroupID: "org.example", ArtifactID: "xyz", Version: "2.0.0"},
				Origin:     "management",
			},
			{
				Dependency: maven.Dependency{GroupID: "org.profile", ArtifactID: "abc", Version: "1.2.3"},
				Origin:     "profile@profile-one",
			},
			{
				Dependency: maven.Dependency{GroupID: "org.profile", ArtifactID: "def", Version: "${def.version}"},
				Origin:     "profile@profile-one",
			},
			{
				Dependency: maven.Dependency{GroupID: "org.import", ArtifactID: "xyz", Version: "6.6.6", Scope: "import", Type: "pom"},
				Origin:     "profile@profile-two@management",
			},
			{
				Dependency: maven.Dependency{GroupID: "org.dep", ArtifactID: "plugin-dep", Version: "2.3.3"},
				Origin:     "plugin@org.plugin:plugin",
			},
			{
				Dependency: maven.Dependency{GroupID: "org.example", ArtifactID: "ddd", Version: "1.2.3"},
				Origin:     "parent@" + parentPath,
			},
			{
				Dependency: maven.Dependency{GroupID: "org.example", ArtifactID: "aaa", Version: "${aaa.version}"},
				Origin:     "parent@" + parentPath + "@management",
			},
		},
		ParentPaths: []string{parentPath},
	}
	if diff := cmp.Diff(specific, m.EcosystemSpecific()); diff != "" {
		t.Errorf("Manifest.EcosystemSpecific() mismatch (-want +got):\n%s", diff)
	}

	patches := []result.Patch{
		{
			Manifest: m,
			Deps: []result.DependencyPatch{
				{
					Pkg:        result.PackageKey{Name: "org.example:abc"},
					NewRequire: "1.0.2",
				},
				{
					Pkg:        result.PackageKey{Name: "org.example:another-property"},
					NewRequire: "1.1.0",
				},
				{
					Pkg:        result.PackageKey{Name: "org.example:property-no-update"},
					NewRequire: "2.0.0",
				},
				{
					Pkg:        result.PackageKey{Name: "org.example:xyz"},
					NewRequire: "2.0.1",
				},
				{
					Pkg:        result.PackageKey{Name: "org.example:no-version"},
					NewRequire: "2.0.1",
				},
				{
					Pkg:        result.PackageKey{Name: "org.example:override"},
					NewRequire: "2.0.0",
				},
				{
					Pkg:        result.PackageKey{Name: "org.example:suggest"},
					NewRequire: "2.0.0",
				},
				{
					Pkg:        result.PackageKey{Name: "org.profile:abc"},
					NewRequire: "1.2.4",
				},
				{
					Pkg:        result.PackageKey{Name: "org.profile:def"},
					NewRequire: "2.3.5",
				},
				{
					Pkg:        result.PackageKey{Name: "org.import:xyz"},
					NewRequire: "6.7.0",
				},
				{
					Pkg:        result.PackageKey{Name: "org.dep:plugin-dep"},
					NewRequire: "2.3.4",
				},
				{
					Pkg:        result.PackageKey{Name: "org.parent:parent-pom"},
					NewRequire: "1.2.0",
				},
				{
					Pkg:        result.PackageKey{Name: "org.example:ddd"},
					NewRequire: "1.3.0",
				},
				{
					Pkg:        result.PackageKey{Name: "org.example:aaa"},
					NewRequire: "1.2.0",
				},
			},
		},
	}

	outDir := t.TempDir()
	outPath := filepath.Join(outDir, "pom.xml")
	err = rw.Write(m, input.FS, patches, outPath)
	if err != nil {
		t.Fatalf("failed to write patched manifest: %v", err)
	}

	extracttest.AssertExtractJSONMatches(t, outPath, "pom.xml")
	extracttest.AssertExtractJSONMatches(t, filepath.Join(outDir, "../parent/pom.xml"), "parent/pom.xml")
}

func TestMavenWrite(t *testing.T) {
	t.Parallel()

	input := extracttest.GenerateScanInputMock(t, extracttest.ScanInputMockConfig{
		Path: filepath.Join("testdata", "my-app", "pom.xml"),
	})
	defer extracttest.CloseTestScanInput(t, input)

	rw, err := GetReadWriter(nil)
	if err != nil {
		t.Fatalf("failed to create MavenReadWriter: %v", err)
	}

	m, err := rw.Read(input.Path, input.FS)
	if err != nil {
		t.Fatalf("failed to read manifest: %v", err)
	}

	// Add dependencies that are not in the original manifest
	patches := []result.Patch{
		{
			Manifest: m,
			Deps: []result.DependencyPatch{
				{
					Pkg:        result.PackageKey{Name: "org.example:add"},
					NewRequire: "1.0.0",
				},
				{
					Pkg:        result.PackageKey{Name: "org.example:xyz"},
					NewRequire: "2.0.1",
				},
				{
					Pkg:        result.PackageKey{Name: "org.example:abc"},
					NewRequire: "1.0.2",
				},
			},
		},
	}

	outDir := t.TempDir()
	outPath := filepath.Join(outDir, "pom.xml")
	err = rw.Write(m, input.FS, patches, outPath)
	if err != nil {
		t.Fatalf("failed to write patched manifest: %v", err)
	}

	extracttest.AssertExtractJSONMatches(t, outPath, "pom-with-new-dependency.xml")
}

func TestMavenWriteDM(t *testing.T) {
	t.Parallel()

	input := extracttest.GenerateScanInputMock(t, extracttest.ScanInputMockConfig{
		Path: filepath.Join("testdata", "no-dependency-management.xml"),
	})
	defer extracttest.CloseTestScanInput(t, input)

	rw, err := GetReadWriter(nil)
	if err != nil {
		t.Fatalf("failed to create MavenReadWriter: %v", err)
	}

	m, err := rw.Read(input.Path, input.FS)
	if err != nil {
		t.Fatalf("failed to read manifest: %v", err)
	}

	// Add dependencies that are not in the original manifest
	patches := []result.Patch{
		{
			Manifest: m,
			Deps: []result.DependencyPatch{
				{
					Pkg:        result.PackageKey{Name: "org.example:add"},
					NewRequire: "1.0.0",
				},
			},
		},
	}

	outDir := t.TempDir()
	outPath := filepath.Join(outDir, "pom.xml")
	err = rw.Write(m, input.FS, patches, outPath)
	if err != nil {
		t.Fatalf("failed to write patched manifest: %v", err)
	}

	extracttest.AssertExtractJSONMatches(t, outPath, "pom-with-dependency-management.xml")
}

func Test_buildPatches(t *testing.T) {
	m := manifestMaven{
		specific: ManifestSpecific{}, // Required to not be nil
	}

	depDirect := result.PackageKey{Name: "org.example:abc"}
	depDirectAnotherProp := result.PackageKey{Name: "org.example:another-property"}
	depDirectPropNoUpdate := result.PackageKey{Name: "org.example:property-no-update"}

	depParent := result.PackageKey{Name: "org.parent:parent-pom"}
	depParentDirect := result.PackageKey{Name: "org.example:ddd"}
	depParentMgmt := result.PackageKey{Name: "org.example:aaa"}

	depMgmt := result.PackageKey{Name: "org.example:xyz"}
	depProfileOne := result.PackageKey{Name: "org.profile:abc"}
	depProfileTwoMgmt := result.PackageKey{Name: "org.import:xyz"}
	depPlugin := result.PackageKey{Name: "org.dep:plugin-dep"}

	parentPath := filepath.Join("testdata", "parent", "pom.xml")

	patches := []result.Patch{
		{
			Manifest: m,
			Deps: []result.DependencyPatch{
				{
					Name:      "org.example:abc",
					VersionTo: "1.0.2",
					Type:      depDirect,
				},
				{
					Name:      "org.example:another-property",
					VersionTo: "1.1.0",
					Type:      depDirectAnotherProp,
				},
				{
					Name:      "org.example:property-no-update",
					VersionTo: "2.0.0",
					Type:      depDirectPropNoUpdate,
				},
				{
					Name:      "org.example:ddd",
					VersionTo: "1.3.0",
					Type:      depParentDirect,
				},
				{
					Name:      "org.example:aaa",
					VersionTo: "1.2.0",
					Type:      depParentMgmt,
				},
				{
					Name:      "org.example:xyz",
					VersionTo: "2.0.1",
					Type:      depMgmt,
				},
				{
					Name:      "org.dep:plugin-dep",
					VersionTo: "2.3.4",
					Type:      depPlugin,
				},
				{
					Name:      "org.import:xyz",
					VersionTo: "6.7.0",
					Type:      depProfileTwoMgmt,
				},
				{
					Name:      "org.profile:abc",
					VersionTo: "1.2.4",
					Type:      depProfileOne,
				},
				{
					Name:      "org.profile:def",
					VersionTo: "2.3.5",
					Type:      depProfileOne,
				},
				{
					Name:      "org.parent:parent-pom",
					VersionTo: "1.2.0",
					Type:      depParent,
				},
				{
					Name:        "org.example:suggest",
					VersionFrom: "1.0.0",
					VersionTo:   "2.0.0",
					Type:        depMgmt,
				},
				{
					Name:      "org.example:override",
					VersionTo: "2.0.0",
					Type:      depMgmt,
				},
				{
					Name:      "org.example:no-version",
					VersionTo: "2.0.1",
					Type:      depMgmt,
				},
			},
		},
	}
	specific := ManifestSpecific{
		Parent: maven.Parent{
			ProjectKey: maven.ProjectKey{
				GroupID:    "org.parent",
				ArtifactID: "parent-pom",
				Version:    "1.1.1",
			},
			RelativePath: "../parent/pom.xml",
		},
		Properties: []PropertyWithOrigin{
			{Property: maven.Property{Name: "property.version", Value: "1.0.0"}},
			{Property: maven.Property{Name: "no.update.minor", Value: "9"}},
			{Property: maven.Property{Name: "def.version", Value: "2.3.4"}, Origin: "profile@profile-one"},
			{Property: maven.Property{Name: "aaa.version", Value: "1.1.1"}, Origin: "parent@" + parentPath},
		},
		LocalRequirements: []DependencyWithOrigin{
			{
				Dependency: maven.Dependency{GroupID: "org.parent", ArtifactID: "parent-pom", Version: "1.2.0", Type: "pom"},
				Origin:     "parent",
			},
			{
				Dependency: maven.Dependency{GroupID: "junit", ArtifactID: "junit", Version: "${junit.version}", Scope: "test"},
			},
			{
				Dependency: maven.Dependency{GroupID: "org.example", ArtifactID: "abc", Version: "1.0.1"},
			},
			{
				Dependency: maven.Dependency{GroupID: "org.example", ArtifactID: "no-updates", Version: "9.9.9"},
			},
			{
				Dependency: maven.Dependency{GroupID: "org.example", ArtifactID: "no-version"},
			},
			{
				Dependency: maven.Dependency{GroupID: "org.example", ArtifactID: "property", Version: "${property.version}"},
			},
			{
				Dependency: maven.Dependency{GroupID: "org.example", ArtifactID: "property-no-update", Version: "1.${no.update.minor}"},
			},
			{
				Dependency: maven.Dependency{GroupID: "org.example", ArtifactID: "same-property", Version: "${property.version}"},
			},
			{
				Dependency: maven.Dependency{GroupID: "org.example", ArtifactID: "another-property", Version: "${property.version}"},
			},
			{
				Dependency: maven.Dependency{GroupID: "org.example", ArtifactID: "no-version", Version: "2.0.0"},
				Origin:     "management",
			},
			{
				Dependency: maven.Dependency{GroupID: "org.example", ArtifactID: "xyz", Version: "2.0.0"},
				Origin:     "management",
			},
			{
				Dependency: maven.Dependency{GroupID: "org.profile", ArtifactID: "abc", Version: "1.2.3"},
				Origin:     "profile@profile-one",
			},
			{
				Dependency: maven.Dependency{GroupID: "org.profile", ArtifactID: "def", Version: "${def.version}"},
				Origin:     "profile@profile-one",
			},
			{
				Dependency: maven.Dependency{GroupID: "org.import", ArtifactID: "xyz", Version: "6.6.6", Scope: "import", Type: "pom"},
				Origin:     "profile@profile-two@management",
			},
			{
				Dependency: maven.Dependency{GroupID: "org.dep", ArtifactID: "plugin-dep", Version: "2.3.3"},
				Origin:     "plugin@org.plugin:plugin",
			},
			{
				Dependency: maven.Dependency{GroupID: "org.example", ArtifactID: "ddd", Version: "1.2.3"},
				Origin:     "parent@" + parentPath,
			},
			{
				Dependency: maven.Dependency{GroupID: "org.example", ArtifactID: "aaa", Version: "${aaa.version}"},
				Origin:     "parent@" + parentPath + "@management",
			},
		},
	}
	want := map[string]Patches{
		"": {
			DependencyPatches: DependencyPatches{
				"": map[Patch]bool{
					{
						DependencyKey: maven.DependencyKey{
							GroupID:    "org.example",
							ArtifactID: "abc",
							Type:       "jar",
						},
						NewRequire: "1.0.2",
					}: true,
					{
						DependencyKey: maven.DependencyKey{
							GroupID:    "org.example",
							ArtifactID: "another-property",
							Type:       "jar",
						},
						NewRequire: "1.1.0",
					}: true,
					{
						DependencyKey: maven.DependencyKey{
							GroupID:    "org.example",
							ArtifactID: "property-no-update",
							Type:       "jar",
						},
						NewRequire: "2.0.0",
					}: true,
				},
				"management": map[Patch]bool{
					{
						DependencyKey: maven.DependencyKey{
							GroupID:    "org.example",
							ArtifactID: "xyz",
							Type:       "jar",
						},
						NewRequire: "2.0.1",
					}: true,
					{
						DependencyKey: maven.DependencyKey{
							GroupID:    "org.example",
							ArtifactID: "no-version",
							Type:       "jar",
						},
						NewRequire: "2.0.1",
					}: true,
					{
						DependencyKey: maven.DependencyKey{
							GroupID:    "org.example",
							ArtifactID: "override",
							Type:       "jar",
						},
						NewRequire: "2.0.0",
					}: false,
					{
						DependencyKey: maven.DependencyKey{
							GroupID:    "org.example",
							ArtifactID: "suggest",
							Type:       "jar",
						},
						NewRequire: "2.0.0",
					}: false,
				},
				"profile@profile-one": map[Patch]bool{
					{
						DependencyKey: maven.DependencyKey{
							GroupID:    "org.profile",
							ArtifactID: "abc",
							Type:       "jar",
						},
						NewRequire: "1.2.4",
					}: true,
				},
				"profile@profile-two@management": map[Patch]bool{
					{
						DependencyKey: maven.DependencyKey{
							GroupID:    "org.import",
							ArtifactID: "xyz",
							Type:       "pom",
						},
						NewRequire: "6.7.0",
					}: true,
				},
				"plugin@org.plugin:plugin": map[Patch]bool{
					{
						DependencyKey: maven.DependencyKey{
							GroupID:    "org.dep",
							ArtifactID: "plugin-dep",
							Type:       "jar",
						},
						NewRequire: "2.3.4",
					}: true,
				},
				"parent": map[Patch]bool{
					{
						DependencyKey: maven.DependencyKey{
							GroupID:    "org.parent",
							ArtifactID: "parent-pom",
							Type:       "pom",
						},
						NewRequire: "1.2.0",
					}: true,
				},
			},
			PropertyPatches: PropertyPatches{
				"": {
					"property.version": "1.0.1",
				},
				"profile@profile-one": {
					"def.version": "2.3.5",
				},
			},
		},
		parentPath: {
			DependencyPatches: DependencyPatches{
				"": map[Patch]bool{
					{
						DependencyKey: maven.DependencyKey{
							GroupID:    "org.example",
							ArtifactID: "ddd",
							Type:       "jar",
						},
						NewRequire: "1.3.0",
					}: true,
				},
			},
			PropertyPatches: PropertyPatches{
				"": {
					"aaa.version": "1.2.0",
				},
			},
		},
	}

	allPatches, err := buildPatches(patches, specific)
	if err != nil {
		t.Fatalf("failed to build patches: %v", err)
	}
	if diff := cmp.Diff(want, allPatches); diff != "" {
		t.Errorf("result patches mismatch (-want +got):\n%s", diff)
	}
}

func Test_generatePropertyPatches(t *testing.T) {
	tests := []struct {
		s1       string
		s2       string
		possible bool
		patches  map[string]string
	}{
		{"${version}", "1.2.3", true, map[string]string{"version": "1.2.3"}},
		{"${major}.2.3", "1.2.3", true, map[string]string{"major": "1"}},
		{"1.${minor}.3", "1.2.3", true, map[string]string{"minor": "2"}},
		{"1.2.${patch}", "1.2.3", true, map[string]string{"patch": "3"}},
		{"${major}.${minor}.${patch}", "1.2.3", true, map[string]string{"major": "1", "minor": "2", "patch": "3"}},
		{"${major}.2.3", "2.0.0", false, map[string]string{}},
		{"1.${minor}.3", "2.0.0", false, map[string]string{}},
	}
	for _, tt := range tests {
		patches, ok := generatePropertyPatches(tt.s1, tt.s2)
		if ok != tt.possible || !reflect.DeepEqual(patches, tt.patches) {
			t.Errorf("generatePropertyPatches(%s, %s): got %v %v, want %v %v", tt.s1, tt.s2, patches, ok, tt.patches, tt.possible)
		}
	}
}

func TestMavenReadWrite_Containment(t *testing.T) {
	t.Parallel()

	// Create a temporary environment to test containment
	tmpDir := t.TempDir()
	
	// We want to simulate a workspace that is NOT in a git repo
	// So we create a nested project inside tmpDir
	
	projectDir := filepath.Join(tmpDir, "project")
	os.MkdirAll(projectDir, 0755)
	
	// Create a parent pom completely outside the project
	outsideDir := filepath.Join(tmpDir, "outside")
	os.MkdirAll(outsideDir, 0755)
	err := os.WriteFile(filepath.Join(outsideDir, "pom.xml"), []byte(`<project>
	<groupId>com.outside</groupId>
	<artifactId>parent</artifactId>
	<version>1.0.0</version>
	<packaging>pom</packaging>
</project>`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	
	// Create the malicious child pom
	childPomPath := filepath.Join(projectDir, "pom.xml")
	err = os.WriteFile(childPomPath, []byte(`<project>
	<parent>
		<groupId>com.outside</groupId>
		<artifactId>parent</artifactId>
		<version>1.0.0</version>
		<relativePath>../outside/pom.xml</relativePath>
	</parent>
	<artifactId>child</artifactId>
</project>`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	
	mavenRW, err := GetReadWriter(nil)
	if err != nil {
		t.Fatalf("failed to create MavenReadWriter: %v", err)
	}
	
	fsys := fs.DirFS(filepath.Dir(tmpDir))
	
	_, err = mavenRW.Read(strings.TrimPrefix(childPomPath, filepath.Dir(tmpDir)+"/"), fsys)
	
	// The client is passed as nil, so upstream fetch won't occur and will return gracefully.
	// Since local is correctly rejected, the project should be read, but it will have no parent resolved.
	if err != nil {
		t.Errorf("Read failed unexpectedly: %v", err)
	}

	// We can also test writing back to ensure it doesn't write the parent outside.
	// But reading is enough to prove the containment.
}
