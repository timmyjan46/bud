package budfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/livebud/bud/package/budfs/linkmap"

	"github.com/livebud/bud/package/virtual/vcache"

	"github.com/livebud/bud/package/virtual"

	"github.com/livebud/bud/internal/dsync"
	"github.com/livebud/bud/internal/glob"
	"github.com/livebud/bud/internal/once"
	"github.com/livebud/bud/internal/orderedset"
	"github.com/livebud/bud/internal/valid"
	"github.com/livebud/bud/package/budfs/mergefs"
	"github.com/livebud/bud/package/budfs/treefs"
	"github.com/livebud/bud/package/log"
)

func New(fsys fs.FS, log log.Interface) *FileSystem {
	cache := vcache.New()
	node := treefs.New(".")
	merged := mergefs.Merge(node, fsys)
	return &FileSystem{
		cache:  cache,
		closer: new(once.Closer),
		fsys:   merged,
		node:   node,
		log:    log,
		lmap:   linkmap.New(log),
	}
}

type FileSystem struct {
	cache  vcache.Cache
	closer *once.Closer
	fsys   fs.FS
	node   *treefs.Node
	lmap   *linkmap.Map
	log    log.Interface
}

type File struct {
	Data   []byte
	node   *treefs.Node
	target string
}

func (f *File) Target() string {
	return f.target
}

func (f *File) Relative() string {
	return relativePath(f.node.Path(), f.target)
}

func (f *File) Path() string {
	return f.node.Path()
}

func (f *File) Mode() fs.FileMode {
	return f.node.Mode()
}

type FS interface {
	fs.FS
	fs.ReadDirFS
	fs.GlobFS
	Link(to string)
	Context() context.Context
	Defer(func() error)
}

type Dir struct {
	fsys   *FileSystem
	node   *treefs.Node
	target string
}

func (d *Dir) Target() string {
	return d.target
}

func (d *Dir) Relative() string {
	return relativePath(d.node.Path(), d.target)
}

func (d *Dir) Path() string {
	return d.node.Path()
}

func (d *Dir) Mode() fs.FileMode {
	return d.node.Mode()
}

func (d *Dir) GenerateFile(path string, fn func(fsys FS, file *File) error) {
	fileg := &fileGenerator{d.fsys, fn, nil}
	fileg.node = d.node.FileGenerator(path, fileg)
}

func (d *Dir) FileGenerator(path string, generator FileGenerator) {
	d.GenerateFile(path, generator.GenerateFile)
}

func (d *Dir) GenerateDir(dir string, fn func(fsys FS, dir *Dir) error) {
	dirg := &dirGenerator{d.fsys, fn, nil}
	dirg.node = d.node.DirGenerator(dir, dirg)
}

func (d *Dir) DirGenerator(dir string, generator DirGenerator) {
	d.GenerateDir(dir, generator.GenerateDir)
}

type mountGenerator struct {
	dir  string
	fsys fs.FS
}

func (g *mountGenerator) Generate(target string) (fs.File, error) {
	return g.fsys.Open(relativePath(g.dir, target))
}

func (d *Dir) Mount(mount fs.FS) error {
	des, err := fs.ReadDir(mount, ".")
	if err != nil {
		return fmt.Errorf("budfs: mount error. %w", err)
	}
	// Wrap mount in the existing generator cache
	mountg := &mountGenerator{d.node.Path(), mount}
	// Loop over the first level and add the mount, allowing us to mount "."
	// on an existing directory
	for _, de := range des {
		if de.IsDir() {
			d.node.DirGenerator(de.Name(), mountg)
			continue
		}
		d.node.FileGenerator(de.Name(), mountg)
	}
	return nil
}

type FileGenerator interface {
	GenerateFile(fsys FS, file *File) error
}

type GenerateFile func(fsys FS, file *File) error

func (fn GenerateFile) GenerateFile(fsys FS, file *File) error {
	return fn(fsys, file)
}

type DirGenerator interface {
	GenerateDir(fsys FS, dir *Dir) error
}

type GenerateDir func(fsys FS, dir *Dir) error

func (fn GenerateDir) GenerateDir(fsys FS, dir *Dir) error {
	return fn(fsys, dir)
}

type EmbedFile struct {
	Data []byte
}

var _ FileGenerator = (*EmbedFile)(nil)

func (e *EmbedFile) GenerateFile(fsys FS, file *File) error {
	file.Data = e.Data
	return nil
}

func (f *FileSystem) Open(name string) (fs.File, error) {
	file, err := f.fsys.Open(name)
	if err != nil {
		return nil, fmt.Errorf("budfs: open %q. %w", name, err)
	}
	return file, nil
}

func (f *FileSystem) Close() error {
	return f.closer.Close()
}

type fileGenerator struct {
	fsys *FileSystem
	fn   func(fsys FS, file *File) error
	node *treefs.Node
}

func (g *fileGenerator) Generate(target string) (fs.File, error) {
	if entry, ok := g.fsys.cache.Get(target); ok {
		return virtual.New(entry), nil
	}
	fctx := &fileSystem{context.TODO(), g.fsys, g.fsys.lmap.Scope(target)}
	file := &File{nil, g.node, target}
	g.fsys.log.Debug("budfs: running file generator function", "target", target)
	if err := g.fn(fctx, file); err != nil {
		return nil, err
	}
	vfile := &virtual.File{
		Path: g.node.Path(),
		Mode: g.node.Mode(),
		Data: file.Data,
	}
	g.fsys.cache.Set(target, vfile)
	return virtual.New(vfile), nil
}

func (f *FileSystem) GenerateFile(path string, fn func(fsys FS, file *File) error) {
	fileg := &fileGenerator{f, fn, nil}
	fileg.node = f.node.FileGenerator(path, fileg)
}

func (f *FileSystem) FileGenerator(path string, generator FileGenerator) {
	f.GenerateFile(path, generator.GenerateFile)
}

type dirGenerator struct {
	fsys *FileSystem
	fn   func(fsys FS, dir *Dir) error
	node *treefs.Node
}

func (g *dirGenerator) Generate(target string) (fs.File, error) {
	if _, ok := g.fsys.cache.Get(g.node.Path()); ok {
		return g.node.Open(target)
	}
	fctx := &fileSystem{context.TODO(), g.fsys, g.fsys.lmap.Scope(target)}
	dir := &Dir{g.fsys, g.node, target}
	g.fsys.log.Debug("budfs: running dir generator function", "path", g.node.Path(), "target", target)
	if err := g.fn(fctx, dir); err != nil {
		return nil, err
	}
	g.fsys.cache.Set(g.node.Path(), &virtual.Dir{
		Path:    g.node.Path(),
		Mode:    g.node.Mode(),
		Entries: g.node.Entries(),
	})
	return g.node.Open(target)
}

func (f *FileSystem) GenerateDir(path string, fn func(fsys FS, dir *Dir) error) {
	dirg := &dirGenerator{f, fn, nil}
	dirg.node = f.node.DirGenerator(path, dirg)
}

func (f *FileSystem) DirGenerator(path string, generator DirGenerator) {
	f.GenerateDir(path, generator.GenerateDir)
}

type fileServer struct {
	fsys *FileSystem
	fn   func(fsys FS, file *File) error
	node *treefs.Node
}

func (g *fileServer) Generate(target string) (fs.File, error) {
	if entry, ok := g.fsys.cache.Get(target); ok {
		return virtual.New(entry), nil
	}
	rel := relativePath(g.node.Path(), target)
	if rel == "." {
		return nil, &fs.PathError{
			Op:   "open",
			Path: g.node.Path(),
			Err:  fs.ErrInvalid,
		}
	}
	fctx := &fileSystem{context.TODO(), g.fsys, g.fsys.lmap.Scope(target)}
	// File differs slightly than others because g.node.Path() is the directory
	// path, but we want the target path for serving files.
	file := &File{nil, g.node, target}
	g.fsys.log.Debug("budfs: running file server function", "path", g.node.Path(), "target", target)
	if err := g.fn(fctx, file); err != nil {
		return nil, err
	}
	vfile := &virtual.File{
		Path: target,
		Mode: fs.FileMode(0),
		Data: file.Data,
	}
	g.fsys.cache.Set(target, vfile)
	return virtual.New(vfile), nil
}

func (f *FileSystem) ServeFile(dir string, fn func(fsys FS, file *File) error) {
	fileg := &fileServer{f, fn, nil}
	fileg.node = f.node.DirGenerator(dir, fileg)
}

func (f *FileSystem) FileServer(dir string, generator FileGenerator) {
	f.ServeFile(dir, generator.GenerateFile)
}

// Sync the overlay to the filesystem
func (f *FileSystem) Sync(writable virtual.FS, to string) error {
	// Temporarily replace the underlying fs.FS with a cached fs.FS
	cache := vcache.New()
	fsys := f.fsys
	f.fsys = vcache.Wrap(cache, fsys, f.log)
	err := dsync.To(f.fsys, writable, to)
	f.fsys = fsys
	return err
}

// Change updates the cache
func (f *FileSystem) Change(paths ...string) {
	for i := 0; i < len(paths); i++ {
		path := paths[i]
		if f.cache.Has(path) {
			f.log.Debug("budfs: cache", "delete", path)
			f.cache.Delete(path)
		}
		f.lmap.Range(func(genPath string, fns *linkmap.List) bool {
			if f.cache.Has(genPath) && fns.Check(path) {
				paths = append(paths, genPath)
			}
			return true
		})
	}
}

type fileSystem struct {
	ctx  context.Context
	fsys *FileSystem
	link *linkmap.List
}

var _ FS = (*fileSystem)(nil)

// Open implements fs.FS
func (f *fileSystem) Open(name string) (fs.File, error) {
	file, err := f.fsys.Open(name)
	if err != nil {
		return nil, err
	}
	f.link.Link("open", name)
	return file, nil
}

func (f *fileSystem) Link(to string) {
	f.link.Link("link", to)
}

func (f *fileSystem) Context() context.Context {
	return f.ctx
}

// Defer a function until close is called. May be called multiple times if
// generators are triggered multiple times.
func (f *fileSystem) Defer(fn func() error) {
	f.fsys.closer.Closes = append(f.fsys.closer.Closes, fn)
}

// Glob implements fs.GlobFS
func (f *fileSystem) Glob(pattern string) (matches []string, err error) {
	// Compile the pattern into a glob matcher
	matcher, err := glob.Compile(pattern)
	if err != nil {
		return nil, err
	}
	// Watch for changes to the pattern
	f.link.Select("glob", func(path string) bool {
		return matcher.Match(path)
	})
	// Base is a minor optimization to avoid walking the entire tree
	bases, err := glob.Bases(pattern)
	if err != nil {
		return nil, err
	}
	// Compute the matches for each base
	for _, base := range bases {
		results, err := f.glob(matcher, base)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		matches = append(matches, results...)
	}
	// Deduplicate the matches
	return orderedset.Strings(matches...), nil
}

// ReadDir implements fs.ReadDirFS
func (f *fileSystem) ReadDir(name string) ([]fs.DirEntry, error) {
	des, err := fs.ReadDir(f.fsys, name)
	if err != nil {
		return nil, err
	}
	f.link.Select("readdir", func(path string) bool {
		return path == name || filepath.Dir(path) == name
	})
	return des, nil
}

func (f *fileSystem) glob(matcher glob.Matcher, base string) (matches []string, err error) {
	// Walk the directory tree, filtering out non-valid paths
	err = fs.WalkDir(f.fsys, base, valid.WalkDirFunc(func(path string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// If the paths match, add it to the list of matches
		if matcher.Match(path) {
			matches = append(matches, path)
		}
		return nil
	}))
	if err != nil {
		return nil, err
	}
	// return the list of matches
	return matches, nil
}

func relativePath(base, target string) string {
	rel := strings.TrimPrefix(target, base)
	if rel == "" {
		return "."
	} else if rel[0] == '/' {
		rel = rel[1:]
	}
	return rel
}
