package build

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	motherrelease "github.com/osman-yahya/feast-watch/mother/release"
	"github.com/osman-yahya/feast-watch/shared/release"
)

// Store reads the build catalogue and serves out of it.
//
// It is both halves of what the mother needs from a release host: the index of
// what versions exist (release.Source) and the bytes themselves — the same two
// jobs GitHub used to do, answered from a directory instead.
//
// There is no database behind it. The filesystem is the catalogue: a version
// exists exactly when its directory does, and Build makes that directory appear
// complete or not at all. Nothing can drift out of step with the files, because
// there is nothing else to drift.
type Store struct{ dir string }

func New(dir string) *Store { return &Store{dir: dir} }

// Ensure resolves an asset that has been built. Unlike a mirror it fetches
// nothing: if the version was never built here, it does not exist.
func (s *Store) Ensure(version, asset string) (string, error) {
	path := filepath.Join(s.dir, version, asset)
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("%s of %s has not been built on this mother", asset, version)
	}
	return path, nil
}

// Fetch implements release.Source by reading the catalogue, so the index, the
// API and the panel all keep working against a mother that publishes its own
// builds. notModified is always false: scanning a handful of directories is
// cheaper than the bookkeeping to avoid it.
func (s *Store) Fetch(context.Context) (agents, mother []motherrelease.Build, notModified bool, err error) {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		// Nothing built yet is an empty catalogue, not a fault.
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		agentPlats, motherPlats := s.platformsOf(e.Name())
		if len(agentPlats) > 0 {
			agents = append(agents, motherrelease.Build{Version: e.Name(), Platforms: agentPlats})
		}
		if len(motherPlats) > 0 {
			mother = append(mother, motherrelease.Build{Version: e.Name(), Platforms: motherPlats})
		}
	}
	motherrelease.SortDescending(agents)
	motherrelease.SortDescending(mother)
	return agents, mother, false, nil
}

// platformsOf reports which builds a version directory actually holds, applying
// the rule the GitHub index applied: a binary with no checksum beside it is a
// rollout target that could only ever fail verification, so it is not offered.
func (s *Store) platformsOf(version string) (agents, mother []string) {
	files, err := os.ReadDir(filepath.Join(s.dir, version))
	if err != nil {
		return nil, nil
	}
	present := make(map[string]bool, len(files))
	for _, f := range files {
		present[f.Name()] = true
	}
	for name := range present {
		kind, plat, ok := release.AssetKindOf(name)
		if !ok || !present[name+release.ChecksumSuffix] {
			continue
		}
		switch kind {
		case release.KindAgent:
			agents = append(agents, plat)
		case release.KindMother:
			mother = append(mother, plat)
		}
	}
	sortStrings(agents)
	sortStrings(mother)
	return agents, mother
}

// sortStrings keeps platform lists in a stable order, so two reads of the same
// catalogue are the same answer.
func sortStrings(in []string) {
	sort.Strings(in)
}
