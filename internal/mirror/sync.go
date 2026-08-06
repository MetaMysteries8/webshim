package mirror

import (
	"context"
	"fmt"
	"time"

	"github.com/MetaMysteries8/webshim/internal/websim"
)

// SyncResult records what a pull brought down.
type SyncResult struct {
	ProjectID string   `json:"project_id"`
	Version   int      `json:"version"`
	Files     []string `json:"files"`
	Skipped   []string `json:"skipped,omitempty"`
}

// SyncDown replaces the mirror with the contents of the project's live
// revision and writes a fresh manifest.
//
// index.html comes from the raw content host along with every other asset. Any
// file that cannot be downloaded is recorded in Skipped rather than aborting the
// sync: one unreadable asset should not block work on the rest, but it must not
// silently look like it synced either.
func (m *Mirror) SyncDown(ctx context.Context, client *websim.Client, projectID string) (*SyncResult, error) {
	project, err := client.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	version, err := project.RequireCurrentVersion()
	if err != nil {
		return nil, err
	}

	assets, err := client.ListAssets(ctx, projectID, version)
	if err != nil {
		return nil, err
	}

	// The asset listing does not include the homepage, which is managed
	// through POST /sites, so request it explicitly.
	paths := []string{websim.IndexPath}
	for _, a := range assets {
		if websim.IsIndexPath(a.Path) {
			continue
		}
		paths = append(paths, a.Path)
	}

	result := &SyncResult{ProjectID: projectID, Version: version}
	files := make(map[string]string, len(paths))

	for _, p := range paths {
		content, err := client.ReadFile(ctx, projectID, p, version)
		if err != nil {
			result.Skipped = append(result.Skipped, fmt.Sprintf("%s (%v)", p, err))
			continue
		}
		if err := m.Write(p, content); err != nil {
			result.Skipped = append(result.Skipped, fmt.Sprintf("%s (%v)", p, err))
			continue
		}
		files[p] = hashBytes(content)
		result.Files = append(result.Files, p)
	}

	if len(result.Files) == 0 {
		return nil, fmt.Errorf("sync: nothing could be downloaded from project %s v%d", projectID, version)
	}

	if err := m.WriteManifest(&Manifest{
		ProjectID: projectID,
		Version:   version,
		SyncedAt:  time.Now(),
		Files:     files,
	}); err != nil {
		return nil, err
	}
	return result, nil
}

// PublishPlan is what a mirror publish would do.
type PublishPlan struct {
	Diff    *Diff
	Request websim.PublishRequest

	// LiveVersion is the project's current version at plan time.
	LiveVersion int

	// SyncedVersion is the version the manifest was built from.
	SyncedVersion int
}

// Plan builds a publish request from the mirror's local changes.
//
// It refuses when the live revision has moved past the manifest. Publishing a
// diff computed against an older parent would silently revert whatever changed
// in between -- the same hazard the client's rebase guard catches server-side,
// caught earlier here where the fix ("sync, then re-apply") is cheaper.
func (m *Mirror) Plan(ctx context.Context, client *websim.Client, projectID, description string) (*PublishPlan, error) {
	diff, man, err := m.Diff()
	if err != nil {
		return nil, err
	}
	if man.ProjectID != "" && man.ProjectID != projectID {
		return nil, fmt.Errorf("mirror: this directory was synced from project %s, not %s",
			man.ProjectID, projectID)
	}

	project, err := client.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	live, err := project.RequireCurrentVersion()
	if err != nil {
		return nil, err
	}
	if live != man.Version {
		return nil, fmt.Errorf("%w: the mirror was synced from v%d but v%d is live; "+
			"sync again and re-apply your changes",
			ErrStale, man.Version, live)
	}

	if diff.Empty() {
		return nil, fmt.Errorf("mirror: no local changes to publish")
	}

	req := websim.PublishRequest{
		ProjectID:   projectID,
		Description: description,
		Deletes:     diff.Deleted,
	}
	for _, p := range append(append([]string{}, diff.Added...), diff.Modified...) {
		content, err := m.Read(p)
		if err != nil {
			return nil, err
		}
		req.Writes = append(req.Writes, websim.Change{Path: p, Content: content})
	}
	// index.html first, then the rest alphabetically, so the edit history
	// reads sensibly.
	sortChanges(req.Writes)

	return &PublishPlan{
		Diff:          diff,
		Request:       req,
		LiveVersion:   live,
		SyncedVersion: man.Version,
	}, nil
}

// Publish applies a plan and, on success, updates the manifest so the mirror is
// clean again.
func (m *Mirror) Publish(ctx context.Context, client *websim.Client, plan *PublishPlan) (*websim.PublishResult, error) {
	result, err := client.Publish(ctx, plan.Request)
	if err != nil {
		return nil, err
	}

	// The publish landed, so the mirror now matches the new live revision.
	// Rebuilding the manifest from what is on disk keeps the next diff honest.
	files, err := m.hashes()
	if err != nil {
		return result, fmt.Errorf("published v%d successfully, but the manifest could not be rebuilt: %w",
			result.CurrentVersion, err)
	}
	if err := m.WriteManifest(&Manifest{
		ProjectID: plan.Request.ProjectID,
		Version:   result.CurrentVersion,
		SyncedAt:  time.Now(),
		Files:     files,
	}); err != nil {
		return result, fmt.Errorf("published v%d successfully, but the manifest could not be written: %w",
			result.CurrentVersion, err)
	}
	return result, nil
}

// sortChanges puts index.html first, then sorts by path.
func sortChanges(changes []websim.Change) {
	less := func(i, j int) bool {
		iIndex := websim.IsIndexPath(changes[i].Path)
		jIndex := websim.IsIndexPath(changes[j].Path)
		if iIndex != jIndex {
			return iIndex
		}
		return changes[i].Path < changes[j].Path
	}
	// A simple insertion sort: change sets are small, and this avoids
	// pulling in sort.Slice's reflection for a handful of elements.
	for i := 1; i < len(changes); i++ {
		for j := i; j > 0 && less(j, j-1); j-- {
			changes[j], changes[j-1] = changes[j-1], changes[j]
		}
	}
}
