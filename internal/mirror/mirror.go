// Package mirror is a local working copy of a WebSim project.
//
// Without it, every iteration would have to push a whole file through the API
// just to see a change, and the model would have to re-emit entire documents to
// make small edits. With it, the agent edits ordinary files on disk and one
// publish uploads only what actually changed, wrapped in a single transaction.
//
// A manifest records the content hash of every file as of the last sync, which
// is what makes "only what changed" both cheap and correct: no re-downloading
// the live revision to diff against it, and no guessing from file sizes.
package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MetaMysteries8/webshim/internal/websim"
)

// ManifestName is the sync record inside a mirror directory. The leading dot
// keeps it out of publishes, which skip dotfiles.
const ManifestName = ".webshim-manifest.json"

// MaxFileBytes caps a single mirror file, matching the client's asset limit.
const MaxFileBytes = websim.MaxAssetBytes

// ErrNotSynced means the mirror has no manifest, so there is nothing to diff
// against.
var ErrNotSynced = errors.New("mirror: not synced yet")

// ErrStale means the live revision moved since the last sync.
var ErrStale = errors.New("mirror: out of date with the live revision")

// Manifest records what the mirror contained at the last sync.
type Manifest struct {
	ProjectID string            `json:"project_id"`
	Version   int               `json:"version"`
	SyncedAt  time.Time         `json:"synced_at"`
	Files     map[string]string `json:"files"` // project-relative path -> sha256 hex
}

// Mirror is a directory holding one project's files.
type Mirror struct {
	// Dir is the mirror's root on disk.
	Dir string

	// root confines every file operation to Dir, so a symlink cannot be used
	// to read or write outside it.
	root *os.Root
}

// Open opens or creates a mirror directory.
func Open(dir string) (*Mirror, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mirror: creating %s: %w", dir, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("mirror: opening %s: %w", dir, err)
	}
	return &Mirror{Dir: dir, root: root}, nil
}

// Close releases the mirror's directory handle.
func (m *Mirror) Close() error {
	if m.root == nil {
		return nil
	}
	return m.root.Close()
}

// resolve validates a project-relative path for use inside the mirror.
//
// It applies the same rules as the API client, so a path that cannot be
// published also cannot be written locally. The manifest is off limits: it is
// bookkeeping, not project content.
func resolve(p string) (string, error) {
	clean, err := websim.ValidatePath(p)
	if err != nil {
		return "", err
	}
	if path.Base(clean) == ManifestName {
		return "", fmt.Errorf("%w: %s is webshim's own sync record", websim.ErrUnsafePath, ManifestName)
	}
	return clean, nil
}

// Read returns the contents of a mirror file.
func (m *Mirror) Read(p string) ([]byte, error) {
	clean, err := resolve(p)
	if err != nil {
		return nil, err
	}
	data, err := m.root.ReadFile(clean)
	if err != nil {
		return nil, fmt.Errorf("mirror: reading %s: %w", clean, err)
	}
	return data, nil
}

// Write creates or replaces a mirror file, creating parent directories.
func (m *Mirror) Write(p string, content []byte) error {
	clean, err := resolve(p)
	if err != nil {
		return err
	}
	if int64(len(content)) > MaxFileBytes {
		return fmt.Errorf("mirror: %s is %d bytes, over the %d byte limit", clean, len(content), MaxFileBytes)
	}
	if dir := path.Dir(clean); dir != "." {
		if err := m.root.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mirror: creating %s: %w", dir, err)
		}
	}
	if err := m.root.WriteFile(clean, content, 0o644); err != nil {
		return fmt.Errorf("mirror: writing %s: %w", clean, err)
	}
	return nil
}

// Delete removes a mirror file.
func (m *Mirror) Delete(p string) error {
	clean, err := resolve(p)
	if err != nil {
		return err
	}
	if err := m.root.Remove(clean); err != nil {
		return fmt.Errorf("mirror: deleting %s: %w", clean, err)
	}
	return nil
}

// Entry describes one file in the mirror.
type Entry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// List returns every publishable file in the mirror, sorted by path.
//
// Dotfiles and dot-directories are excluded, which is what keeps the manifest,
// and anything like a stray .env, out of a publish.
func (m *Mirror) List() ([]Entry, error) {
	var out []Entry
	err := fs.WalkDir(m.root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := path.Base(p)
		if p != "." && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, Entry{Path: p, Size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("mirror: listing %s: %w", m.Dir, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// hashes returns the sha256 of every file currently in the mirror.
func (m *Mirror) hashes() (map[string]string, error) {
	entries, err := m.List()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		data, err := m.root.ReadFile(e.Path)
		if err != nil {
			return nil, fmt.Errorf("mirror: hashing %s: %w", e.Path, err)
		}
		out[e.Path] = hashBytes(data)
	}
	return out, nil
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ReadManifest loads the sync record.
func (m *Mirror) ReadManifest() (*Manifest, error) {
	data, err := m.root.ReadFile(ManifestName)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotSynced
	}
	if err != nil {
		return nil, fmt.Errorf("mirror: reading the manifest: %w", err)
	}
	var man Manifest
	if err := json.Unmarshal(data, &man); err != nil {
		// A corrupt manifest is recoverable by re-syncing, so say that.
		return nil, fmt.Errorf("mirror: the manifest is unreadable (%v); re-sync to rebuild it", err)
	}
	if man.Files == nil {
		man.Files = map[string]string{}
	}
	return &man, nil
}

// WriteManifest saves the sync record.
func (m *Mirror) WriteManifest(man *Manifest) error {
	data, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return err
	}
	if err := m.root.WriteFile(ManifestName, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("mirror: writing the manifest: %w", err)
	}
	return nil
}

// Diff is the set of changes between the mirror and its last sync.
type Diff struct {
	// Added are paths present in the mirror but not in the manifest.
	Added []string `json:"added,omitempty"`

	// Modified are paths whose content hash changed.
	Modified []string `json:"modified,omitempty"`

	// Deleted are paths in the manifest that are gone from the mirror.
	Deleted []string `json:"deleted,omitempty"`

	// Unchanged are paths whose content matches the manifest.
	Unchanged []string `json:"unchanged,omitempty"`
}

// Empty reports whether there is nothing to publish.
func (d Diff) Empty() bool {
	return len(d.Added) == 0 && len(d.Modified) == 0 && len(d.Deleted) == 0
}

// Summary is a one-line description for a permission prompt or status line.
func (d Diff) Summary() string {
	if d.Empty() {
		return "no changes"
	}
	var parts []string
	if n := len(d.Added); n > 0 {
		parts = append(parts, fmt.Sprintf("%d added", n))
	}
	if n := len(d.Modified); n > 0 {
		parts = append(parts, fmt.Sprintf("%d modified", n))
	}
	if n := len(d.Deleted); n > 0 {
		parts = append(parts, fmt.Sprintf("%d deleted", n))
	}
	return strings.Join(parts, ", ")
}

// ChangedPaths returns everything that would be written or removed.
func (d Diff) ChangedPaths() []string {
	out := append([]string{}, d.Added...)
	out = append(out, d.Modified...)
	out = append(out, d.Deleted...)
	sort.Strings(out)
	return out
}

// Diff compares the mirror against its manifest.
func (m *Mirror) Diff() (*Diff, *Manifest, error) {
	man, err := m.ReadManifest()
	if err != nil {
		return nil, nil, err
	}
	current, err := m.hashes()
	if err != nil {
		return nil, nil, err
	}

	d := &Diff{}
	for p, h := range current {
		prior, existed := man.Files[p]
		switch {
		case !existed:
			d.Added = append(d.Added, p)
		case prior != h:
			d.Modified = append(d.Modified, p)
		default:
			d.Unchanged = append(d.Unchanged, p)
		}
	}
	for p := range man.Files {
		if _, still := current[p]; !still {
			d.Deleted = append(d.Deleted, p)
		}
	}

	sort.Strings(d.Added)
	sort.Strings(d.Modified)
	sort.Strings(d.Deleted)
	sort.Strings(d.Unchanged)
	return d, man, nil
}

// LocalPath returns the on-disk path for a project-relative path. It is for
// display only; all I/O goes through the confined root.
func (m *Mirror) LocalPath(p string) string {
	return filepath.Join(m.Dir, filepath.FromSlash(p))
}
