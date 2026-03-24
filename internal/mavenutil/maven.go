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

// Package mavenutil provides utility functions for Maven projects.
package mavenutil

import (
	"context"
	"os"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"deps.dev/util/maven"
	"deps.dev/util/resolve"
	"deps.dev/util/semver"
	"github.com/google/osv-scalibr/clients/datasource"
	"github.com/google/osv-scalibr/extractor/filesystem"
)

const MaxParent = 100

// Origin indicators for dependencies and properties
const (
	OriginManagement = "management"
	OriginParent     = "parent"
	OriginPlugin     = "plugin"
	OriginProfile    = "profile"
)

// Options holds options for getting dependency management.
// Input indicates where the current maven project originates.
// It is used to get parent from local file system when needed.
// AddRegistry indicates whether to add the Maven repositories in the project to the client.
// AllowLocal indicates whether to allow parsing parent pom.xml locally.
// InitialParentIndex indicates the index of the first parent project.
// The value of InitialParentIndex is usually 0 if we fetch the parent from
// the project itself, but it can be 1 if the parent comes from the import dependency.
type Options struct {
	Input              *filesystem.ScanInput
	Client             *datasource.MavenRegistryAPIClient
	AddRegistry        bool
	AllowLocal         bool
	InitialParentIndex int
}

// MergeParents parses local accessible parent pom.xml or fetches it from
// upstream, merges into root project, then interpolate the properties.
//   - current holds the current parent project to merge.
//   - result holds the Maven project to merge into, this is modified in place.
//   - opts holds the options for merging parent data.
func MergeParents(ctx context.Context, current maven.Parent, result *maven.Project, opts Options) error {
	currentPath := ""
	rootPath := ""
	if opts.Input != nil {
		currentPath = opts.Input.Path
		rootPath = FindProjectRoot(currentPath)
	}

	allowLocal := opts.AllowLocal
	visited := make(map[maven.ProjectKey]struct{}, MaxParent)
	for n := opts.InitialParentIndex; n < MaxParent; n++ {
		if current.GroupID == "" || current.ArtifactID == "" || current.Version == "" {
			break
		}
		if _, ok := visited[current.ProjectKey]; ok {
			// A cycle of parents is detected
			return errors.New("a cycle of parents is detected")
		}
		visited[current.ProjectKey] = struct{}{}

		var proj maven.Project
		parentFoundLocally := false
		if allowLocal {
			var parentPath string
			var err error
			parentFoundLocally, parentPath, err = loadParentLocal(opts.Input, current, currentPath, rootPath, &proj)
			if err != nil {
				return fmt.Errorf("failed to load parent at %s: %w", currentPath, err)
			}
			if parentPath != "" {
				currentPath = parentPath
			}
		}
		if !parentFoundLocally {
			// Once we fetch a parent pom.xml from upstream, we should not
			// allow parsing parent pom.xml locally anymore.
			allowLocal = false
			var err error
			proj, err = loadParentRemote(ctx, opts.Client, current, n)
			if err != nil {
				return fmt.Errorf("failed to load parent from remote: %w", err)
			}
		}
		// Use an empty JDK string and ActivationOS here to merge the default profiles.
		if err := result.MergeProfiles("", maven.ActivationOS{}); err != nil {
			return fmt.Errorf("failed to merge default profiles: %w", err)
		}
		// Interpolate the repositories in the project to get rid of the placeholders in URLs.
		if err := proj.InterpolateRepositories(); err != nil {
			return fmt.Errorf("failed to interpolate repositories: %w", err)
		}
		if opts.Client != nil && opts.AddRegistry && len(proj.Repositories) > 0 {
			for _, repo := range proj.Repositories {
				if err := opts.Client.AddRegistry(ctx, datasource.MavenRegistry{
					URL:              string(repo.URL),
					ID:               string(repo.ID),
					ReleasesEnabled:  repo.Releases.Enabled.Boolean(),
					SnapshotsEnabled: repo.Snapshots.Enabled.Boolean(),
				}); err != nil {
					return fmt.Errorf("failed to add registry %s: %w", repo.URL, err)
				}
			}
		}
		result.MergeParent(proj)
		current = proj.Parent
	}
	// Interpolate the project to resolve the properties.
	return result.Interpolate()
}

// loadParentLocal loads a parent Maven project from local file system
// and returns whether parent is found locally as well as parent path.
func loadParentLocal(input *filesystem.ScanInput, parent maven.Parent, path string, rootPath string, result *maven.Project) (bool, string, error) {
	parentPath := ParentPOMPath(input, path, string(parent.RelativePath), rootPath)
	if parentPath == "" {
		return false, "", nil
	}
	f, err := input.FS.Open(parentPath)
	if err != nil {
		return false, "", fmt.Errorf("failed to open parent file %s: %w", parentPath, err)
	}
	err = datasource.NewMavenDecoder(f).Decode(result)
	if closeErr := f.Close(); closeErr != nil {
		return false, "", fmt.Errorf("failed to close file: %w", err)
	}
	if err != nil {
		return false, "", fmt.Errorf("failed to unmarshal project: %w", err)
	}
	if ProjectKey(*result) != parent.ProjectKey || result.Packaging != "pom" {
		// Only mark parent as found when the identifiers and packaging are expected.
		return false, "", nil
	}
	return true, parentPath, nil
}

// loadParentRemote loads a parent from remote registry.
func loadParentRemote(ctx context.Context, mavenClient *datasource.MavenRegistryAPIClient, parent maven.Parent, parentIndex int) (maven.Project, error) {
	if mavenClient == nil {
		// The client is not available, so return an empty project.
		return maven.Project{}, nil
	}

	proj, err := mavenClient.GetProject(ctx, string(parent.GroupID), string(parent.ArtifactID), string(parent.Version))
	if err != nil {
		return maven.Project{}, fmt.Errorf("failed to get Maven project %s:%s:%s: %w", parent.GroupID, parent.ArtifactID, parent.Version, err)
	}
	if parentIndex > 0 && proj.Packaging != "pom" {
		// A parent project should only be of "pom" packaging type.
		return maven.Project{}, fmt.Errorf("invalid packaging for parent project %s", proj.Packaging)
	}
	if ProjectKey(proj) != parent.ProjectKey {
		// The identifiers in parent does not match what we want.
		return maven.Project{}, fmt.Errorf("parent identifiers mismatch: %v, expect %v", proj.ProjectKey, parent.ProjectKey)
	}
	return proj, nil
}

// ProjectKey returns a project key with empty groupId/version
// filled by corresponding fields in parent.
func ProjectKey(proj maven.Project) maven.ProjectKey {
	if proj.GroupID == "" {
		proj.GroupID = proj.Parent.GroupID
	}
	if proj.Version == "" {
		proj.Version = proj.Parent.Version
	}

	return proj.ProjectKey
}


// FindProjectRoot attempts to find the root of the project by walking up the directory
// tree looking for a .git directory. If it reaches the root of the filesystem without
// finding one, it falls back to the directory of the initial path.
func FindProjectRoot(startPath string) string {
	absPath, err := filepath.Abs(startPath)
	if err != nil {
		return filepath.Dir(startPath)
	}

	dir := filepath.Dir(absPath)
	for {
		if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir { // Reached filesystem root
			break
		}
		dir = parent
	}

	return filepath.Dir(absPath)
}

// IsWithinRoot checks if targetPath is within rootPath.
func IsWithinRoot(rootPath, targetPath string) bool {
	rootAbs, err := filepath.Abs(rootPath)
	if err != nil {
		return false
	}
	targetAbs, err := filepath.Abs(targetPath)
	if err != nil {
		return false
	}

	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		return false
	}

	// Clean the path to remove extraneous . or ..
	rel = filepath.Clean(rel)

	if rel == ".." || strings.HasPrefix(rel, ".." + string(filepath.Separator)) {
		return false
	}

	return true
}

// ParentPOMPath returns the path of a parent pom.xml.
// Maven looks for the parent POM first in 'relativePath', then
// the local repository '../pom.xml', and lastly in the remote repo.
// An empty string is returned if failed to resolve the parent path.
func ParentPOMPath(input *filesystem.ScanInput, currentPath, relativePath, rootPath string) string {
	if relativePath == "" {
		relativePath = "../pom.xml"
	}

	path := filepath.ToSlash(filepath.Join(filepath.Dir(currentPath), relativePath))
	if info, err := input.FS.Stat(path); err == nil {
		if !info.IsDir() {
			if rootPath != "" && !IsWithinRoot(rootPath, path) {
				return ""
			}
			return path
		}
		// Current path is a directory, so look for pom.xml in the directory.
		path = filepath.ToSlash(filepath.Join(path, "pom.xml"))
		if _, err := input.FS.Stat(path); err == nil {
			if rootPath != "" && !IsWithinRoot(rootPath, path) {
				return ""
			}
			return path
		}
	}

	return ""
}

// GetDependencyManagement returns managed dependencies in the specified Maven project by fetching remote pom.xml.
func GetDependencyManagement(ctx context.Context, client *datasource.MavenRegistryAPIClient, groupID, artifactID, version maven.String) (maven.DependencyManagement, error) {
	root := maven.Parent{ProjectKey: maven.ProjectKey{GroupID: groupID, ArtifactID: artifactID, Version: version}}
	var result maven.Project
	// To get dependency management from another project, we need the
	// project with parents merged, so we call MergeParents by passing
	// an empty project.
	if err := MergeParents(ctx, root, &result, Options{
		Client:             client,
		AddRegistry:        false,
		AllowLocal:         false,
		InitialParentIndex: 0,
	}); err != nil {
		return maven.DependencyManagement{}, err
	}

	return result.DependencyManagement, nil
}

// CompareVersions compares two Maven semver versions with special behaviour for specific packages,
// producing more desirable ordering using non-standard comparison.
func CompareVersions(vk resolve.VersionKey, a *semver.Version, b *semver.Version) int {
	if a == nil || b == nil {
		if a == nil {
			return -1
		}

		return 1
	}

	if vk.Name == "com.google.guava:guava" {
		// com.google.guava:guava has 'flavors' with versions ending with -jre or -android.
		// https://github.com/google/guava/wiki/ReleasePolicy#flavors
		// To preserve the flavor in updates, we make the opposite flavor considered the earliest versions.

		// Old versions have '22.0' and '22.0-android', and even older version don't have any flavors.
		// Only check for the android flavor, and assume its jre otherwise.
		wantAndroid := strings.HasSuffix(vk.Version, "-android")

		aIsAndroid := strings.HasSuffix(a.String(), "-android")
		bIsAndroid := strings.HasSuffix(b.String(), "-android")

		if aIsAndroid == bIsAndroid {
			return a.Compare(b)
		}

		if aIsAndroid == wantAndroid {
			return 1
		}

		return -1
	}

	// Old versions of apache commons-* libraries (commons-io:commons-io, commons-math:commons-math, etc.)
	// used date-based versions (e.g. 20040118.003354), which naturally sort after the more recent semver versions.
	// We manually force the date versions to come before the others to prevent downgrades.
	if strings.HasPrefix(vk.Name, "commons-") {
		// All date-based versions of these packages seem to be in the years 2002-2005.
		// It's extremely unlikely we'd see any versions dated before 1999 or after 2010.
		// It's also unlikely we'd see any major versions of these packages reach up to 200.0.0.
		// Checking if the version starts with "200" should therefore be sufficient to determine if it's a year.
		aCal := strings.HasPrefix(a.String(), "200")
		bCal := strings.HasPrefix(b.String(), "200")

		if aCal == bCal {
			return a.Compare(b)
		}

		if aCal {
			return -1
		}

		return 1
	}

	return a.Compare(b)
}

// IsPrerelease returns whether the given version is a prerelease version.
// There is a special handling for com.google.guava:guava, which has 'flavors' with versions ending
// with '-jre' or '-android'. These versions are not considered as prerelease versions.
func IsPrerelease(ver *semver.Version, vk resolve.VersionKey) bool {
	if vk.Name == "com.google.guava:guava" {
		return false
	}
	return ver.IsPrerelease()
}
