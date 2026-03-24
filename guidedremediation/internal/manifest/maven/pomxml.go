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

// Package maven provides the manifest parsing and writing for the Maven pom.xml format.
package maven

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"deps.dev/util/maven"
	"deps.dev/util/resolve"
	"deps.dev/util/resolve/dep"
	"github.com/google/osv-scalibr/clients/datasource"
	"github.com/google/osv-scalibr/extractor/filesystem"
	scalibrfs "github.com/google/osv-scalibr/fs"
	"github.com/google/osv-scalibr/guidedremediation/internal/manifest"
	"github.com/google/osv-scalibr/guidedremediation/result"
	"github.com/google/osv-scalibr/guidedremediation/strategy"
	"github.com/google/osv-scalibr/internal/mavenutil"
	forkedxml "github.com/google/osv-scalibr/internal/xml"
)

type property struct {
	Name   string
	Value  string
	Origin string
}

func (p property) Empty() bool {
	return p.Name == "" && p.Value == "" && p.Origin == ""
}

type dependency struct {
	Type       string     `xml:"type,omitempty"`
	Classifier string     `xml:"classifier,omitempty"`
	Scope      string     `xml:"scope,omitempty"`
	SystemPath string     `xml:"systemPath,omitempty"`
	Exclusions []struct{} `xml:"exclusions>exclusion,omitempty"` // empty struct to skip writing empty elements
	Optional   string     `xml:"optional,omitempty"`
	GroupID    string     `xml:"groupId"`
	ArtifactID string     `xml:"artifactId"`
	Version    string     `xml:"version,omitempty"`
}

func makeDependency(p Patch) dependency {
	return dependency{
		GroupID:    p.GroupID,
		ArtifactID: p.ArtifactID,
		Version:    p.NewRequire,
		Type:       p.Type,
		Classifier: p.Classifier,
	}
}

func compareDependency(a, b dependency) int {
	if c := cmp.Compare(a.GroupID, b.GroupID); c != 0 {
		return c
	}
	if c := cmp.Compare(a.ArtifactID, b.ArtifactID); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Type, b.Type); c != 0 {
		return c
	}
	return cmp.Compare(a.Classifier, b.Classifier)
}

type dependencyManagement struct {
	Dependencies []dependency `xml:"dependencies>dependency"`
}

type PropertyWithOrigin struct {
	maven.Property

	Origin string // Origin indicates where the property comes from
}

type DependencyWithOrigin struct {
	maven.Dependency

	Origin string // Origin indicates where the dependency comes from
}

type ManifestSpecific struct {
	Parent            maven.Parent
	Properties        []PropertyWithOrigin
	LocalRequirements []DependencyWithOrigin
	ParentPaths       []string
}

func getRequirements(project maven.Project) []resolve.RequirementVersion {
	var requirements []resolve.RequirementVersion
	if project.Parent.GroupID != "" && project.Parent.ArtifactID != "" {
		requirements = append(requirements, makeRequirementVersion(maven.Dependency{
			GroupID:    project.Parent.GroupID,
			ArtifactID: project.Parent.ArtifactID,
			Version:    project.Parent.Version,
			Type:       "pom",
		}, mavenutil.OriginParent))
	}
	for _, d := range project.Dependencies {
		requirements = append(requirements, makeRequirementVersion(d, ""))
	}
	for _, d := range project.DependencyManagement.Dependencies {
		requirements = append(requirements, makeRequirementVersion(d, mavenutil.OriginManagement))
	}

	return requirements
}

func makeRequirementVersion(d maven.Dependency, origin string) resolve.RequirementVersion {
	typ := mavenutil.MavenDepType(d, origin)
	if d.Version == "" {
		typ.AddAttr(dep.MavenKnownAsEmptyVersion, "")
	}

	return resolve.RequirementVersion{
		VersionKey: resolve.VersionKey{
			PackageKey: resolve.PackageKey{
				System: resolve.Maven,
				Name:   d.Name(),
			},
			VersionType: resolve.Requirement,
			Version:     string(d.Version),
		},
		Type: typ,
	}
}

// TODO: combine PropertyPatches and DependencyPatches into one struct
type PropertyPatches map[string]map[string]string // Origin -> map[property name] -> new value
type DependencyPatches map[string]map[Patch]bool  // Origin -> map[Patch] -> updated

type Patches struct {
	DependencyPatches DependencyPatches
	PropertyPatches   PropertyPatches
}

type Patch struct {
	maven.DependencyKey
	NewRequire string
}

// mavenOrigin returns a combined origin string from origin segments.
// Consecutive empty strings are omitted.
func mavenOrigin(os ...string) string {
	return strings.Join(slices.DeleteFunc(os, func(o string) bool {
		return o == ""
	}), "@")
}

// parseOrigin parses a combined origin string into origin segments.
// The returned segments are exactly three: prefix, profile or plugin, and management.
// The prefix is the origin string of where the profile/plugin/management is found.
// The profile or plugin is the id of the profile or plugin, if the origin string indicates so.
// The management is OriginManagement if the origin string indicates so.
func parseOrigin(o string) (prefix, pp, mgmt string) {
	if strings.HasSuffix(o, "@"+mavenutil.OriginManagement) || o == mavenutil.OriginManagement {
		mgmt = mavenutil.OriginManagement
		o = strings.TrimSuffix(o, "@"+mavenutil.OriginManagement)
		o = strings.TrimSuffix(o, mavenutil.OriginManagement)
	}

	i := strings.LastIndex(o, "@"+mavenutil.OriginProfile+"@")
	if i >= 0 {
		prefix = o[:i]
		pp = strings.TrimPrefix(o[i+1:], mavenutil.OriginProfile+"@")
		return prefix, pp, mgmt
	}
	if strings.HasPrefix(o, mavenutil.OriginProfile+"@") {
		pp = strings.TrimPrefix(o, mavenutil.OriginProfile+"@")
		return prefix, pp, mgmt
	}

	i = strings.LastIndex(o, "@"+mavenutil.OriginPlugin+"@")
	if i >= 0 {
		prefix = o[:i]
		pp = strings.TrimPrefix(o[i+1:], mavenutil.OriginPlugin+"@")
		return prefix, pp, mgmt
	}
	if strings.HasPrefix(o, mavenutil.OriginPlugin+"@") {
		pp = strings.TrimPrefix(o, mavenutil.OriginPlugin+"@")
		return prefix, pp, mgmt
	}

	prefix = o
	return prefix, pp, mgmt
}

func buildOriginalRequirements(project maven.Project, originPrefix string) []DependencyWithOrigin {
	var dependencies []DependencyWithOrigin
	if project.Parent.GroupID != "" && project.Parent.ArtifactID != "" {
		dependencies = append(dependencies, DependencyWithOrigin{
			Dependency: maven.Dependency{
				GroupID:    project.Parent.GroupID,
				ArtifactID: project.Parent.ArtifactID,
				Version:    project.Parent.Version,
				Type:       "pom",
			},
			Origin: mavenOrigin(originPrefix, mavenutil.OriginParent),
		})
	}
	for _, d := range project.Dependencies {
		dependencies = append(dependencies, DependencyWithOrigin{Dependency: d, Origin: originPrefix})
	}
	for _, d := range project.DependencyManagement.Dependencies {
		dependencies = append(dependencies, DependencyWithOrigin{
			Dependency: d,
			Origin:     mavenOrigin(originPrefix, mavenutil.OriginManagement),
		})
	}
	for _, profile := range project.Profiles {
		origin := mavenOrigin(originPrefix, mavenutil.OriginProfile, string(profile.ID))
		for _, d := range profile.Dependencies {
			dependencies = append(dependencies, DependencyWithOrigin{Dependency: d, Origin: origin})
		}
		for _, d := range profile.DependencyManagement.Dependencies {
			dependencies = append(dependencies, DependencyWithOrigin{
				Dependency: d,
				Origin:     mavenOrigin(origin, mavenutil.OriginManagement),
			})
		}
	}
	for _, plugin := range project.Build.PluginManagement.Plugins {
		origin := mavenOrigin(originPrefix, mavenutil.OriginPlugin, plugin.Name())
		for _, d := range plugin.Dependencies {
			dependencies = append(dependencies, DependencyWithOrigin{Dependency: d, Origin: origin})
		}
	}
	for _, plugin := range project.Build.Plugins {
		origin := mavenOrigin(originPrefix, mavenutil.OriginPlugin, plugin.Name())
		for _, d := range plugin.Dependencies {
			dependencies = append(dependencies, DependencyWithOrigin{Dependency: d, Origin: origin})
		}
	}

	return dependencies
}

func buildPropertiesWithOrigins(project maven.Project, originPrefix string) []PropertyWithOrigin {
	count := len(project.Properties.Properties)
	for _, prof := range project.Profiles {
		count += len(prof.Properties.Properties)
	}
	properties := make([]PropertyWithOrigin, 0, count)
	for _, prop := range project.Properties.Properties {
		properties = append(properties, PropertyWithOrigin{Property: prop, Origin: originPrefix})
	}
	for _, profile := range project.Profiles {
		for _, prop := range profile.Properties.Properties {
			properties = append(properties, PropertyWithOrigin{
				Property: prop,
				Origin:   mavenOrigin(originPrefix, mavenutil.OriginProfile, string(profile.ID)),
			})
		}
	}

	return properties
}

func (r readWriter) readManifest(path string, fsys scalibrfs.FS) (manifest.Manifest, error) {
	ctx := context.Background()
	path = filepath.ToSlash(path)
	f, err := fsys.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var project maven.Project
	if err := datasource.NewMavenDecoder(f).Decode(&project); err != nil {
		return nil, fmt.Errorf("failed to unmarshal project: %w", err)
	}
	properties := buildPropertiesWithOrigins(project, "")
	origRequirements := buildOriginalRequirements(project, "")

	var reqsForUpdates []resolve.RequirementVersion
	if project.Parent.GroupID != "" && project.Parent.ArtifactID != "" {
		reqsForUpdates = append(reqsForUpdates, makeRequirementVersion(maven.Dependency{
			GroupID:    project.Parent.GroupID,
			ArtifactID: project.Parent.ArtifactID,
			Version:    project.Parent.Version,
			Type:       "pom",
		}, mavenutil.OriginParent))
	}

	// Empty JDK and ActivationOS indicates merging the default profiles.
	if err := project.MergeProfiles("", maven.ActivationOS{}); err != nil {
		return nil, fmt.Errorf("failed to merge profiles: %w", err)
	}

	// Interpolate the repositories in the project to get rid of the placeholders in URLs.
	if err := project.InterpolateRepositories(); err != nil {
		return nil, fmt.Errorf("failed to interpolate repositories: %w", err)
	}
	for _, repo := range project.Repositories {
		if err := r.AddRegistry(ctx, datasource.MavenRegistry{
			URL:              string(repo.URL),
			ID:               string(repo.ID),
			ReleasesEnabled:  repo.Releases.Enabled.Boolean(),
			SnapshotsEnabled: repo.Snapshots.Enabled.Boolean(),
		}); err != nil {
			return nil, fmt.Errorf("failed to add registry %s: %w", repo.URL, err)
		}
	}

	// Merging parents data by parsing local parent pom.xml or fetching from upstream.
	if err := mavenutil.MergeParents(ctx, project.Parent, &project, mavenutil.Options{
		Input:              &filesystem.ScanInput{FS: fsys, Path: path},
		Client:             r.MavenRegistryAPIClient,
		AddRegistry:        true,
		AllowLocal:         true,
		InitialParentIndex: 1,
	}); err != nil {
		return nil, fmt.Errorf("failed to merge parents: %w", err)
	}

	// For dependency management imports, the dependencies that imports
	// dependencies from other projects will be replaced by the imported
	// dependencies, so add them to requirements first.
	for _, dmDep := range project.DependencyManagement.Dependencies {
		if dmDep.Scope == "import" && dmDep.Type == "pom" {
			reqsForUpdates = append(reqsForUpdates, makeRequirementVersion(dmDep, mavenutil.OriginManagement))
		}
	}

	// Process the dependencies:
	//  - dedupe dependencies and dependency management
	//  - import dependency management
	//  - fill in missing dependency version requirement
	project.ProcessDependencies(func(groupID, artifactID, version maven.String) (maven.DependencyManagement, error) {
		return mavenutil.GetDependencyManagement(ctx, r.MavenRegistryAPIClient, groupID, artifactID, version)
	})

	localDeps, localProps, paths, err := getLocalDepsAndProps(fsys, path, project.Parent)
	if err != nil {
		return nil, fmt.Errorf("failed to get local requirements and properties: %w", err)
	}
	origRequirements = append(origRequirements, localDeps...)
	properties = append(properties, localProps...)

	reqsForUpdates = append(reqsForUpdates, getRequirements(project)...)
	var resolvedReqs []resolve.RequirementVersion
	for _, req := range reqsForUpdates {
		if !req.Type.HasAttr(dep.MavenKnownAsEmptyVersion) {
			resolvedReqs = append(resolvedReqs, req)
		}
	}

	m := manifestMaven{
		filePath:     path,
		requirements: resolvedReqs,
		groups:       make(map[result.RequirementKey][]string),
		specific: ManifestSpecific{
			Parent:            project.Parent,
			Properties:        properties,
			LocalRequirements: origRequirements,
			ParentPaths:       paths,
		},
	}
	if err := m.addRequirementsAndGroups(project); err != nil {
		return nil, err
	}

	return m, nil
}

type manifestMaven struct {
	filePath     string
	requirements []resolve.RequirementVersion
	groups       map[result.RequirementKey][]string
	specific     ManifestSpecific
}

func (m manifestMaven) FilePath() string {
	return m.filePath
}

func (m manifestMaven) Requirements() []resolve.RequirementVersion {
	return m.requirements
}

func (m manifestMaven) Groups() map[result.RequirementKey][]string {
	return m.groups
}

func (m manifestMaven) EcosystemSpecific() any {
	return m.specific
}

func (m *manifestMaven) addRequirementsAndGroups(project maven.Project) error {
	m.groups = make(map[result.RequirementKey][]string)

	deps := append([]maven.Dependency{}, project.Dependencies...)
	for _, p := range project.Profiles {
		deps = append(deps, p.Dependencies...)
	}

	for _, d := range deps {
		req := resolve.RequirementVersion{
			VersionKey: resolve.VersionKey{
				PackageKey: resolve.PackageKey{
					System: resolve.Maven,
					Name:   d.Name(),
				},
				VersionType: resolve.Requirement,
				Version:     string(d.Version),
			},
			Type: resolve.MavenDepType(d, ""),
		}
		if d.Scope == "test" || d.Scope == "provided" {
			m.groups[result.MakeRequirementKey(req)] = append(m.groups[result.MakeRequirementKey(req)], "dev")
		}
		m.requirements = append(m.requirements, req)
	}

	return nil
}

type readWriter struct {
	*datasource.MavenRegistryAPIClient
}

// GetReadWriter returns a ReadWriter for pom.xml manifest files.
func GetReadWriter(client *datasource.MavenRegistryAPIClient) (manifest.ReadWriter, error) {
	return readWriter{MavenRegistryAPIClient: client}, nil
}

// System returns the ecosystem of this ReadWriter.
func (r readWriter) System() resolve.System {
	return resolve.Maven
}

// SupportedStrategies returns the remediation strategies supported for this manifest.
func (r readWriter) SupportedStrategies() []strategy.Strategy {
	return []strategy.Strategy{strategy.StrategyOverride}
}

// Read parses the manifest from the given file.
func (r readWriter) Read(path string, fsys scalibrfs.FS) (manifest.Manifest, error) {
	ctx := context.Background()
	path = filepath.ToSlash(path)
	f, err := fsys.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var project maven.Project
	if err := datasource.NewMavenDecoder(f).Decode(&project); err != nil {
		return nil, fmt.Errorf("failed to unmarshal project: %w", err)
	}

	if project.Parent.GroupID != "" && project.Parent.ArtifactID != "" && project.Parent.Version != "" {
		if _, _, err := loadParentLocal(&filesystem.ScanInput{FS: fsys, Path: path}, project.Parent, path, mavenutil.FindProjectRoot(path), &maven.Project{}); err != nil {
			return nil, fmt.Errorf("failed to verify local parent POM: %w", err)
		}
	}

	return r.readManifest(path, fsys)
}

func resultProjectKey(k resolve.PackageKey) maven.ProjectKey {
	i := strings.Index(k.Name, ":")
	return maven.ProjectKey{
		GroupID:    maven.String(k.Name[:i]),
		ArtifactID: maven.String(k.Name[i+1:]),
	}
}

func resultDependencyKey(k result.RequirementKey) maven.DependencyKey {
	i := strings.Index(k.Name, ":")
	typ, _ := k.Type.Attr(dep.MavenArtifactType)
	classifier, _ := k.Type.Attr(dep.MavenArtifactClassifier)
	return maven.DependencyKey{
		GroupID:    maven.String(k.Name[:i]),
		ArtifactID: maven.String(k.Name[i+1:]),
		Type:       maven.String(typ),
		Classifier: maven.String(classifier),
	}
}

// interpolate traverses the string by replacing the properties.
// Properties that cannot be resolved are left intact.
func interpolate(s string, properties map[string]string) string {
	return mavenutil.InterpolateRegex.ReplaceAllStringFunc(s, func(match string) string {
		name := strings.TrimSpace(match[2 : len(match)-1])
		if val, ok := properties[name]; ok {
			return val
		}
		return match
	})
}

// generatePropertyPatches returns a map of property-value pairs that can be applied to s1 to reach s2.
// It returns a boolean indicating whether it is possible.
// Only string with a single property or multiple properties separated by . are supported.
// e.g. s1 = "1.${minor}.3", s2 = "1.2.3" -> map[string]string{"minor": "2"}
func generatePropertyPatches(s1, s2 string) (map[string]string, bool) {
	if s1 == s2 {
		return make(map[string]string), true
	}
	i1, i2 := 0, 0
	patches := make(map[string]string)
	for i1 < len(s1) && i2 < len(s2) {
		if s1[i1] == s2[i2] {
			i1++
			i2++
			continue
		}
		if s1[i1] == '$' && i1+1 < len(s1) && s1[i1+1] == '{' {
			end := strings.Index(s1[i1:], "}")
			if end < 0 {
				return nil, false
			}
			prop := strings.TrimSpace(s1[i1+2 : i1+end])
			i1 += end + 1

			// Properties can only be separated by "."
			next1 := strings.Index(s1[i1:], ".")
			var next2 int
			if next1 < 0 {
				next1 = len(s1)
				next2 = len(s2)
			} else {
				next1 += i1
				next2 = strings.Index(s2[i2:], s1[i1:next1])
				if next2 < 0 {
					return nil, false
				}
				next2 += i2
			}
			patches[prop] = s2[i2:next2]
			i2 = next2
			continue
		}
		return nil, false
	}
	if i1 != len(s1) || i2 != len(s2) {
		return nil, false
	}

	return patches, true
}

func buildPatches(patches []result.Patch, specific ManifestSpecific) (map[string]Patches, error) {
	allPatches := make(map[string]Patches)
	for _, patch := range patches {
		if _, ok := patch.Manifest.EcosystemSpecific().(ManifestSpecific); !ok {
			return nil, errors.New("invalid maven ManifestSpecific data")
		}
		for _, depPatch := range patch.Deps {
			depKey := resultDependencyKey(depPatch.Pkg)
			for _, origReq := range specific.LocalRequirements {
				if origReq.Key() != depKey || string(origReq.Version) != depPatch.OrigRequire {
					continue
				}

				prefix, pp, mgmt := parseOrigin(origReq.Origin)
				if _, ok := allPatches[prefix]; !ok {
					allPatches[prefix] = Patches{
						DependencyPatches: make(DependencyPatches),
						PropertyPatches:   make(PropertyPatches),
					}
				}

				// The origin of the requirement is used to determine where to apply the patch.
				// If the original requirement contains properties, we try to apply patches to the properties instead.
				// But we only patch the properties if the property is defined in the same file.
				// If the property is not defined in the same file, we apply the patch to the requirement.
				var props map[string]string
				possible := false
				if strings.Contains(string(origReq.Version), "$") {
					props, possible = generatePropertyPatches(string(origReq.Version), depPatch.NewRequire)
				}
				patchedProps := false
				if possible && len(props) > 0 {
					patchedProps = true
					for k, v := range props {
						found := false
						for _, prop := range specific.Properties {
							if string(prop.Name) == k && string(prop.Value) != v {
								propPrefix, propPP, _ := parseOrigin(prop.Origin)
								if prefix == propPrefix {
									if _, ok := allPatches[prefix].PropertyPatches[prop.Origin]; !ok {
										allPatches[prefix].PropertyPatches[prop.Origin] = make(map[string]string)
									}
									allPatches[prefix].PropertyPatches[prop.Origin][k] = v
									found = true
								} else {
									// The property is not defined in the same file as the requirement.
									// E.g. The dependency is in a profile, but the property is defined globally.
									// In this case, we prefer to patch the requirement directly.
									// As patching the property globally may affect other dependencies.
									patchedProps = false
									break
								}
							}
						}
						if !found {
							// The property is not found, possibly defined outside the project (e.g. from command line).
							// So we patch the requirement directly.
							patchedProps = false
							break
						}
					}
				}

				if !patchedProps {
					if _, ok := allPatches[prefix].DependencyPatches[origReq.Origin]; !ok {
						allPatches[prefix].DependencyPatches[origReq.Origin] = make(map[Patch]bool)
					}
					// For property-based requirement we cannot patch, we directly replace it with the new requirement.
					allPatches[prefix].DependencyPatches[origReq.Origin][Patch{
						DependencyKey: depKey,
						NewRequire:    depPatch.NewRequire,
					}] = false
				}

				if prefix == "" && pp == "" && mgmt == "" && depKey == specific.Parent.Key() {
					// Originating from the parent
					if _, ok := allPatches[""].DependencyPatches["parent"]; !ok {
						allPatches[""].DependencyPatches["parent"] = make(map[Patch]bool)
					}
					allPatches[""].DependencyPatches["parent"][Patch{
						DependencyKey: depKey,
						NewRequire:    depPatch.NewRequire,
					}] = false
				}
			}
		}
	}

	return allPatches, nil
}

// TODO: refactor MergeParents to return local requirements and properties
func getLocalDepsAndProps(fsys scalibrfs.FS, path string, parent maven.Parent) ([]DependencyWithOrigin, []PropertyWithOrigin, []string, error) {
	var localDeps []DependencyWithOrigin
	var localProps []PropertyWithOrigin

	// Walk through local parent pom.xml for original dependencies and properties.
	currentPath := path
	rootPath := mavenutil.FindProjectRoot(currentPath)
	visited := make(map[maven.ProjectKey]bool, mavenutil.MaxParent)
	paths := []string{currentPath}
	for range mavenutil.MaxParent {
		if parent.GroupID == "" || parent.ArtifactID == "" || parent.Version == "" {
			break
		}
		if visited[parent.ProjectKey] {
			// A cycle of parents is detected
			return nil, nil, nil, errors.New("a cycle of parents is detected")
		}
		visited[parent.ProjectKey] = true

		currentPath = mavenutil.ParentPOMPath(&filesystem.ScanInput{FS: fsys}, currentPath, string(parent.RelativePath), rootPath)
		if currentPath == "" {
			// No more local parent pom.xml exists.
			break
		}

		f, err := fsys.Open(currentPath)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to open parent file %s: %w", currentPath, err)
		}

		var proj maven.Project
		err = datasource.NewMavenDecoder(f).Decode(&proj)
		f.Close()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to unmarshal project: %w", err)
		}
		if mavenutil.ProjectKey(proj) != parent.ProjectKey || proj.Packaging != "pom" {
			// This is not the project that we are looking for, we should fetch from upstream
			// that we don't have write access so we give up here.
			break
		}

		origin := mavenOrigin(mavenutil.OriginParent, currentPath)
		localDeps = append(localDeps, buildOriginalRequirements(proj, origin)...)
		localProps = append(localProps, buildPropertiesWithOrigins(proj, origin)...)
		paths = append(paths, currentPath)
		parent = proj.Parent
	}

	return localDeps, localProps, paths, nil
}

// Write writes the manifest after applying the patches to outputPath.
//
// original is the manifest without patches. fsys is the FS that the manifest was read from.
// outputPath is the path on disk (*not* in fsys) to write the entire patched manifest to (this can overwrite the original manifest).
//
// If the original manifest referenced local parent POMs, they will be written alongside the patched manifest, maintaining the relative path structure as it existed in the original location.
func (r readWriter) Write(original manifest.Manifest, fsys scalibrfs.FS, patches []result.Patch, outputPath string) error {
	specific, ok := original.EcosystemSpecific().(ManifestSpecific)
	if !ok {
		return errors.New("invalid maven ManifestSpecific data")
	}

	allPatches, err := buildPatches(patches, specific)
	if err != nil {
		return err
	}

	for _, patchPath := range specific.ParentPaths {
		patches := allPatches[patchPath]
		if patchPath == original.FilePath() {
			patches = allPatches[""]
		}
		depFile, err := fsys.Open(patchPath)
		if err != nil {
			return err
		}
		in := new(bytes.Buffer)
		if _, err := in.ReadFrom(depFile); err != nil {
			depFile.Close()
			return fmt.Errorf("failed to read from DepFile: %w", err)
		}
		depFile.Close()

		var out bytes.Buffer
		if err := write(in.String(), &out, patches); err != nil {
			return fmt.Errorf("failed to write patched manifest: %w", err)
		}

		path := patchPath
		if path == original.FilePath() {
			path = outputPath
		} else {
			rel, err := filepath.Rel(filepath.Dir(original.FilePath()), path)
			if err != nil {
				return fmt.Errorf("failed to get relative path: %w", err)
			}
			path = filepath.Join(filepath.Dir(outputPath), rel)
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", filepath.Dir(path), err)
			}
		}

		if err := os.WriteFile(path, out.Bytes(), 0644); err != nil {
			return err
		}
	}

	return nil
}

// projectStartElement finds the first tag element in the xml string.
func projectStartElement(s string) string {
	for i, c := range s {
		if c == '<' {
			if strings.HasPrefix(s[i:], "<?") || strings.HasPrefix(s[i:], "<!--") || strings.HasPrefix(s[i:], "<!") {
				continue
			}

			// find the end of the start element
			if end := strings.Index(s[i:], ">"); end > 0 {
				return s[:i+end+1]
			}
			break
		}
	}

	return ""
}

func write(raw string, w io.Writer, patches Patches) error {
	dec := forkedxml.NewDecoder(bytes.NewReader([]byte(raw)))
	enc := forkedxml.NewEncoder(w)

	for {
		token, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("getting token: %w", err)
		}

		if tt, ok := token.(forkedxml.StartElement); ok {
			if tt.Name.Local == "project" {
				type RawProject struct {
					InnerXML string `xml:",innerxml"`
				}
				var rawProj RawProject
				if err := dec.DecodeElement(&rawProj, &tt); err != nil {
					return err
				}

				// xml.EncodeToken writes a start element with its all name spaces.
				// It's very common to have a start project element with a few name spaces in Maven.
				// Thus this would cause a big diff when we try to encode the start element of project.

				// We first capture the raw start element string and write it.
				projectStart := projectStartElement(raw)
				if projectStart == "" {
					return errors.New("unable to get start element of project")
				}
				if _, err := w.Write([]byte(projectStart)); err != nil {
					return fmt.Errorf("writing start element of project: %w", err)
				}

				// Then we update the project by passing the innerXML and name spaces are not passed.
				updated := make(map[string]bool) // origin -> updated
				if err := writeProject(w, enc, rawProj.InnerXML, "", "", patches.DependencyPatches, patches.PropertyPatches, updated); err != nil {
					return fmt.Errorf("updating project: %w", err)
				}

				// Check whether dependency management is updated, if not, add a new section of dependency management.
				if dmPatches := patches.DependencyPatches[mavenutil.OriginManagement]; len(dmPatches) > 0 && !updated[mavenutil.OriginManagement] {
					enc.Indent("  ", "  ")
					var dm dependencyManagement
					for p := range dmPatches {
						dm.Dependencies = append(dm.Dependencies, makeDependency(p))
					}
					// Sort dependency management for consistency in testing.
					slices.SortFunc(dm.Dependencies, compareDependency)
					if err := enc.Encode(dm); err != nil {
						return err
					}
					if _, err := w.Write([]byte("\n\n")); err != nil {
						return err
					}
					enc.Indent("", "")
				}

				// Finally we write the end element of project.
				if _, err := w.Write([]byte("</project>")); err != nil {
					return fmt.Errorf("writing start element of project: %w", err)
				}

				continue
			}
		}
		if err := enc.EncodeToken(token); err != nil {
			return err
		}
		if err := enc.Flush(); err != nil {
			return err
		}
	}

	return nil
}

func writeProject(w io.Writer, enc *forkedxml.Encoder, raw, prefix, id string, patches DependencyPatches, properties PropertyPatches, updated map[string]bool) error {
	dec := forkedxml.NewDecoder(bytes.NewReader([]byte(raw)))
	for {
		token, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}

		if tt, ok := token.(forkedxml.StartElement); ok {
			switch tt.Name.Local {
			case "parent":
				updated["parent"] = true
				type RawParent struct {
					maven.ProjectKey

					InnerXML string `xml:",innerxml"`
				}
				var rawParent RawParent
				if err := dec.DecodeElement(&rawParent, &tt); err != nil {
					return err
				}
				req := string(rawParent.Version)
				if parentPatches, ok := patches["parent"]; ok {
					// There should only be one parent patch
					if len(parentPatches) > 1 {
						return fmt.Errorf("multiple parent patches: %v", parentPatches)
					}
					for k := range parentPatches {
						req = k.NewRequire
					}
				}
				if err := writeString(enc, "<parent>"+rawParent.InnerXML+"</parent>", map[string]string{"version": req}); err != nil {
					return fmt.Errorf("updating parent: %w", err)
				}

				continue
			case "properties":
				type RawProperties struct {
					InnerXML string `xml:",innerxml"`
				}
				var rawProperties RawProperties
				if err := dec.DecodeElement(&rawProperties, &tt); err != nil {
					return err
				}
				if err := writeString(enc, "<properties>"+rawProperties.InnerXML+"</properties>", properties[mavenOrigin(prefix, id)]); err != nil {
					return fmt.Errorf("updating properties: %w", err)
				}

				continue
			case "profile":
				if prefix != "" || id != "" {
					// Skip updating if prefix or id is set to avoid infinite recursion
					break
				}
				type RawProfile struct {
					maven.Profile

					InnerXML string `xml:",innerxml"`
				}
				var rawProfile RawProfile
				if err := dec.DecodeElement(&rawProfile, &tt); err != nil {
					return err
				}
				if err := writeProject(w, enc, "<profile>"+rawProfile.InnerXML+"</profile>", mavenutil.OriginProfile, string(rawProfile.ID), patches, properties, updated); err != nil {
					return fmt.Errorf("updating profile: %w", err)
				}

				continue
			case "plugin":
				if prefix != "" || id != "" {
					// Skip updating if prefix or id is set to avoid infinite recursion
					break
				}
				type RawPlugin struct {
					maven.Plugin

					InnerXML string `xml:",innerxml"`
				}
				var rawPlugin RawPlugin
				if err := dec.DecodeElement(&rawPlugin, &tt); err != nil {
					return err
				}
				if err := writeProject(w, enc, "<plugin>"+rawPlugin.InnerXML+"</plugin>", mavenutil.OriginPlugin, rawPlugin.Name(), patches, properties, updated); err != nil {
					return fmt.Errorf("updating profile: %w", err)
				}

				continue
			case "dependencyManagement":
				type RawDependencyManagement struct {
					maven.DependencyManagement

					InnerXML string `xml:",innerxml"`
				}
				var rawDepMgmt RawDependencyManagement
				if err := dec.DecodeElement(&rawDepMgmt, &tt); err != nil {
					return err
				}
				o := mavenOrigin(prefix, id, mavenutil.OriginManagement)
				updated[o] = true
				dmPatches := patches[o]
				if err := writeDependency(w, enc, "<dependencyManagement>"+rawDepMgmt.InnerXML+"</dependencyManagement>", dmPatches); err != nil {
					return fmt.Errorf("updating dependency management: %w", err)
				}

				continue
			case "dependencies":
				type RawDependencies struct {
					Dependencies []maven.Dependency `xml:"dependencies"`
					InnerXML     string             `xml:",innerxml"`
				}
				var rawDeps RawDependencies
				if err := dec.DecodeElement(&rawDeps, &tt); err != nil {
					return err
				}
				o := mavenOrigin(prefix, id)
				updated[o] = true
				depPatches := patches[o]
				if err := writeDependency(w, enc, "<dependencies>"+rawDeps.InnerXML+"</dependencies>", depPatches); err != nil {
					return fmt.Errorf("updating dependencies: %w", err)
				}

				continue
			}
		}
		if err := enc.EncodeToken(token); err != nil {
			return err
		}
	}

	return enc.Flush()
}

// indentation returns the indentation of the dependency element.
// If dependencies or dependency elements are not found, the default
// indentation (four space) is returned.
func indentation(raw string) string {
	i := strings.Index(raw, "<dependencies>")
	if i < 0 {
		return "    "
	}

	raw = raw[i+len("<dependencies>"):]
	// Find the first dependency element.
	j := strings.Index(raw, "<dependency>")
	if j < 0 {
		return "    "
	}

	raw = raw[:j]
	// Find the last new line and get the space between.
	k := strings.LastIndex(raw, "\n")
	if k < 0 {
		return "    "
	}

	return raw[k+1:]
}

func writeDependency(w io.Writer, enc *forkedxml.Encoder, raw string, patches map[Patch]bool) error {
	dec := forkedxml.NewDecoder(bytes.NewReader([]byte(raw)))
	for {
		token, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}

		if tt, ok := token.(forkedxml.StartElement); ok {
			if tt.Name.Local == "dependencies" {
				// We still need to write the start element <dependencies>
				if err := enc.EncodeToken(token); err != nil {
					return err
				}
				if err := enc.Flush(); err != nil {
					return err
				}

				// Write patches that are not in the base project.
				var deps []dependency
				for p, ok := range patches {
					if !ok {
						deps = append(deps, makeDependency(p))
					}
				}
				if len(deps) == 0 {
					// No dependencies to add
					continue
				}
				// Sort dependencies for consistency in testing.
				slices.SortFunc(deps, compareDependency)

				enc.Indent(indentation(raw), "  ")
				// Write a new line to keep the format.
				if _, err := w.Write([]byte("\n")); err != nil {
					return err
				}
				for _, d := range deps {
					if err := enc.Encode(d); err != nil {
						return err
					}
				}
				enc.Indent("", "")

				continue
			}
			if tt.Name.Local == "dependency" {
				type RawDependency struct {
					maven.Dependency

					InnerXML string `xml:",innerxml"`
				}
				var rawDep RawDependency
				if err := dec.DecodeElement(&rawDep, &tt); err != nil {
					return err
				}
				req := string(rawDep.Version)
				for patch := range patches {
					// A Maven dependency key consists of Type and Classifier together with GroupID and ArtifactID.
					if patch.DependencyKey == rawDep.Key() {
						req = patch.NewRequire
					}
				}
				// xml.EncodeElement writes all empty elements and may not follow the existing format.
				// Passing the innerXML can help to keep the original format.
				if err := writeString(enc, "<dependency>"+rawDep.InnerXML+"</dependency>", map[string]string{"version": req}); err != nil {
					return fmt.Errorf("updating dependency: %w", err)
				}

				continue
			}
		}

		if err := enc.EncodeToken(token); err != nil {
			return err
		}
	}

	return enc.Flush()
}

// writeString writes XML string specified by raw with replacements specified in values.
func writeString(enc *forkedxml.Encoder, raw string, values map[string]string) error {
	dec := forkedxml.NewDecoder(bytes.NewReader([]byte(raw)))
	for {
		token, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if tt, ok := token.(forkedxml.StartElement); ok {
			if value, ok2 := values[tt.Name.Local]; ok2 {
				var str string
				if err := dec.DecodeElement(&str, &tt); err != nil {
					return err
				}
				if err := enc.EncodeElement(value, tt); err != nil {
					return err
				}

				continue
			}
		}
		if err := enc.EncodeToken(token); err != nil {
			return err
		}
	}

	return enc.Flush()
}
