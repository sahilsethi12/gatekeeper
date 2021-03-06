// Copyright 2018 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

// Package bundle implements bundle loading.
package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/open-policy-agent/opa/format"

	"github.com/open-policy-agent/opa/internal/file/archive"
	"github.com/open-policy-agent/opa/internal/merge"
	"github.com/open-policy-agent/opa/metrics"

	"github.com/pkg/errors"

	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/util"
)

// Common file extensions and file names.
const (
	RegoExt      = ".rego"
	WasmFile     = "/policy.wasm"
	manifestExt  = ".manifest"
	dataFile     = "data.json"
	yamlDataFile = "data.yaml"
)

const bundleLimitBytes = (1024 * 1024 * 1024) + 1 // limit bundle reads to 1GB to protect against gzip bombs

// Bundle represents a loaded bundle. The bundle can contain data and policies.
type Bundle struct {
	Manifest Manifest
	Data     map[string]interface{}
	Modules  []ModuleFile
	Wasm     []byte
}

// Manifest represents the manifest from a bundle. The manifest may contain
// metadata such as the bundle revision.
type Manifest struct {
	Revision string    `json:"revision"`
	Roots    *[]string `json:"roots,omitempty"`
}

// Init initializes the manifest. If you instantiate a manifest
// manually, call Init to ensure that the roots are set properly.
func (m *Manifest) Init() {
	if m.Roots == nil {
		defaultRoots := []string{""}
		m.Roots = &defaultRoots
	}
}

// AddRoot adds r to the roots of m. This function is idempotent.
func (m *Manifest) AddRoot(r string) {
	m.Init()
	if !RootPathsContain(*m.Roots, r) {
		*m.Roots = append(*m.Roots, r)
	}
}

// Equal returns true if m is semantically equivalent to other.
func (m Manifest) Equal(other Manifest) bool {

	// This is safe since both are passed by value.
	m.Init()
	other.Init()

	if m.Revision != other.Revision {
		return false
	}

	return m.rootSet().Equal(other.rootSet())
}

// Copy returns a deep copy of the manifest.
func (m Manifest) Copy() Manifest {
	m.Init()
	roots := make([]string, len(*m.Roots))
	copy(roots, *m.Roots)
	m.Roots = &roots
	return m
}

func (m Manifest) String() string {
	m.Init()
	return fmt.Sprintf("<revision: %q, roots: %v>", m.Revision, *m.Roots)
}

func (m Manifest) rootSet() stringSet {
	rs := map[string]struct{}{}

	for _, r := range *m.Roots {
		rs[r] = struct{}{}
	}

	return stringSet(rs)
}

type stringSet map[string]struct{}

func (ss stringSet) Equal(other stringSet) bool {
	if len(ss) != len(other) {
		return false
	}
	for k := range other {
		if _, ok := ss[k]; !ok {
			return false
		}
	}
	return true
}

func (m *Manifest) validateAndInjectDefaults(b Bundle) error {

	m.Init()

	// Validate roots in bundle.
	roots := *m.Roots

	// Standardize the roots (no starting or trailing slash)
	for i := range roots {
		roots[i] = strings.Trim(roots[i], "/")
	}

	for i := 0; i < len(roots)-1; i++ {
		for j := i + 1; j < len(roots); j++ {
			if RootPathsOverlap(roots[i], roots[j]) {
				return fmt.Errorf("manifest has overlapped roots: '%v' and '%v'", roots[i], roots[j])
			}
		}
	}

	// Validate modules in bundle.
	for _, module := range b.Modules {
		found := false
		if path, err := module.Parsed.Package.Path.Ptr(); err == nil {
			for i := range roots {
				if strings.HasPrefix(path, roots[i]) {
					found = true
					break
				}
			}
		}
		if !found {
			return fmt.Errorf("manifest roots %v do not permit '%v' in module '%v'", roots, module.Parsed.Package, module.Path)
		}
	}

	// Validate data in bundle.
	return dfs(b.Data, "", func(path string, node interface{}) (bool, error) {
		path = strings.Trim(path, "/")
		for i := range roots {
			if strings.HasPrefix(path, roots[i]) {
				return true, nil
			}
		}
		if _, ok := node.(map[string]interface{}); ok {
			for i := range roots {
				if strings.HasPrefix(roots[i], path) {
					return false, nil
				}
			}
		}
		return false, fmt.Errorf("manifest roots %v do not permit data at path '/%s' (hint: check bundle directory structure)", roots, path)
	})
}

// ModuleFile represents a single module contained a bundle.
type ModuleFile struct {
	URL    string
	Path   string
	Raw    []byte
	Parsed *ast.Module
}

// Reader contains the reader to load the bundle from.
type Reader struct {
	loader                DirectoryLoader
	includeManifestInData bool
	metrics               metrics.Metrics
	baseDir               string
}

// NewReader is deprecated. Use NewCustomReader instead.
func NewReader(r io.Reader) *Reader {
	return NewCustomReader(NewTarballLoader(r))
}

// NewCustomReader returns a new Reader configured to use the
// specified DirectoryLoader.
func NewCustomReader(loader DirectoryLoader) *Reader {
	nr := Reader{
		loader:  loader,
		metrics: metrics.New(),
	}
	return &nr
}

// IncludeManifestInData sets whether the manifest metadata should be
// included in the bundle's data.
func (r *Reader) IncludeManifestInData(includeManifestInData bool) *Reader {
	r.includeManifestInData = includeManifestInData
	return r
}

// WithMetrics sets the metrics object to be used while loading bundles
func (r *Reader) WithMetrics(m metrics.Metrics) *Reader {
	r.metrics = m
	return r
}

// WithBaseDir sets a base directory for file paths of loaded Rego
// modules. This will *NOT* affect the loaded path of data files.
func (r *Reader) WithBaseDir(dir string) *Reader {
	r.baseDir = dir
	return r
}

// Read returns a new Bundle loaded from the reader.
func (r *Reader) Read() (Bundle, error) {

	var bundle Bundle

	bundle.Data = map[string]interface{}{}

	for {
		f, err := r.loader.NextFile()
		if err == io.EOF {
			break
		}
		if err != nil {
			return bundle, errors.Wrap(err, "bundle read failed")
		}

		var buf bytes.Buffer
		n, err := f.Read(&buf, bundleLimitBytes)
		f.Close() // always close, even on error
		if err != nil && err != io.EOF {
			return bundle, err
		} else if err == nil && n >= bundleLimitBytes {
			return bundle, fmt.Errorf("bundle exceeded max size (%v bytes)", bundleLimitBytes-1)
		}

		// Normalize the paths to use `/` separators
		path := filepath.ToSlash(f.Path())

		if strings.HasSuffix(path, RegoExt) {
			fullPath := r.fullPath(path)
			r.metrics.Timer(metrics.RegoModuleParse).Start()
			module, err := ast.ParseModule(fullPath, buf.String())
			r.metrics.Timer(metrics.RegoModuleParse).Stop()
			if err != nil {
				return bundle, err
			}

			mf := ModuleFile{
				URL:    f.URL(),
				Path:   fullPath,
				Raw:    buf.Bytes(),
				Parsed: module,
			}
			bundle.Modules = append(bundle.Modules, mf)

		} else if path == WasmFile {
			bundle.Wasm = buf.Bytes()

		} else if filepath.Base(path) == dataFile {
			var value interface{}

			r.metrics.Timer(metrics.RegoDataParse).Start()
			err := util.NewJSONDecoder(&buf).Decode(&value)
			r.metrics.Timer(metrics.RegoDataParse).Stop()

			if err != nil {
				return bundle, errors.Wrapf(err, "bundle load failed on %v", r.fullPath(path))
			}

			if err := insertValue(&bundle, path, value); err != nil {
				return bundle, err
			}

		} else if filepath.Base(path) == yamlDataFile {

			var value interface{}

			r.metrics.Timer(metrics.RegoDataParse).Start()
			err := util.Unmarshal(buf.Bytes(), &value)
			r.metrics.Timer(metrics.RegoDataParse).Stop()

			if err != nil {
				return bundle, errors.Wrapf(err, "bundle load failed on %v", r.fullPath(path))
			}

			if err := insertValue(&bundle, path, value); err != nil {
				return bundle, err
			}

		} else if strings.HasSuffix(path, manifestExt) {
			if err := util.NewJSONDecoder(&buf).Decode(&bundle.Manifest); err != nil {
				return bundle, errors.Wrap(err, "bundle load failed on manifest decode")
			}
		}
	}

	if err := bundle.Manifest.validateAndInjectDefaults(bundle); err != nil {
		return bundle, err
	}

	if r.includeManifestInData {
		var metadata map[string]interface{}

		b, err := json.Marshal(&bundle.Manifest)
		if err != nil {
			return bundle, errors.Wrap(err, "bundle load failed on manifest marshal")
		}

		err = util.UnmarshalJSON(b, &metadata)
		if err != nil {
			return bundle, errors.Wrap(err, "bundle load failed on manifest unmarshal")
		}

		// For backwards compatibility always write to the old unnamed manifest path
		// This will *not* be correct if >1 bundle is in use...
		if err := bundle.insertData(legacyManifestStoragePath, metadata); err != nil {
			return bundle, errors.Wrapf(err, "bundle load failed on %v", legacyRevisionStoragePath)
		}
	}

	return bundle, nil
}

func (r *Reader) fullPath(path string) string {
	if r.baseDir != "" {
		path = filepath.Join(r.baseDir, path)
	}
	return path
}

// Write is deprecated. Use NewWriter instead.
func Write(w io.Writer, bundle Bundle) error {
	return NewWriter(w).
		UseModulePath(true).
		DisableFormat(true).
		Write(bundle)
}

// Writer implements bundle serialization.
type Writer struct {
	usePath       bool
	disableFormat bool
	w             io.Writer
}

// NewWriter returns a bundle writer that writes to w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{
		w: w,
	}
}

// UseModulePath configures the writer to use the module file path instead of the
// module file URL during serialization. This is for backwards compatibility.
func (w *Writer) UseModulePath(yes bool) *Writer {
	w.usePath = yes
	return w
}

// DisableFormat configures the writer to just write out raw bytes instead
// of formatting modules before serialization.
func (w *Writer) DisableFormat(yes bool) *Writer {
	w.disableFormat = yes
	return w
}

// Write writes the bundle to the writer's output stream.
func (w *Writer) Write(bundle Bundle) error {
	gw := gzip.NewWriter(w.w)
	tw := tar.NewWriter(gw)

	var buf bytes.Buffer

	if err := json.NewEncoder(&buf).Encode(bundle.Data); err != nil {
		return err
	}

	if err := archive.WriteFile(tw, "data.json", buf.Bytes()); err != nil {
		return err
	}

	for _, module := range bundle.Modules {
		path := module.URL
		if w.usePath {
			path = module.Path
		}

		doFormat := !w.disableFormat
		bs := module.Raw
		if bs == nil {
			var err error
			bs, err = format.Ast(module.Parsed)
			if err != nil {
				return err
			}
			doFormat = false // do not reformat
		}

		if doFormat {
			var err error
			bs, err = format.Source(path, module.Raw)
			if err != nil {
				return err
			}
		}

		if err := archive.WriteFile(tw, path, bs); err != nil {
			return err
		}
	}

	if err := writeWasm(tw, bundle); err != nil {
		return err
	}

	if err := writeManifest(tw, bundle); err != nil {
		return err
	}

	if err := tw.Close(); err != nil {
		return err
	}

	return gw.Close()
}

func writeWasm(tw *tar.Writer, bundle Bundle) error {
	if len(bundle.Wasm) == 0 {
		return nil
	}

	return archive.WriteFile(tw, WasmFile, bundle.Wasm)
}

func writeManifest(tw *tar.Writer, bundle Bundle) error {

	var buf bytes.Buffer

	if err := json.NewEncoder(&buf).Encode(bundle.Manifest); err != nil {
		return err
	}

	return archive.WriteFile(tw, manifestExt, buf.Bytes())
}

// ParsedModules returns a map of parsed modules with names that are
// unique and human readable for the given a bundle name.
func (b *Bundle) ParsedModules(bundleName string) map[string]*ast.Module {

	mods := make(map[string]*ast.Module, len(b.Modules))

	for _, mf := range b.Modules {
		mods[modulePathWithPrefix(bundleName, mf.Path)] = mf.Parsed
	}

	return mods
}

// Equal returns true if this bundle's contents equal the other bundle's
// contents.
func (b Bundle) Equal(other Bundle) bool {
	if !reflect.DeepEqual(b.Data, other.Data) {
		return false
	}
	if len(b.Modules) != len(other.Modules) {
		return false
	}
	for i := range b.Modules {
		if b.Modules[i].URL != other.Modules[i].URL {
			return false
		}
		if b.Modules[i].Path != other.Modules[i].Path {
			return false
		}
		if !b.Modules[i].Parsed.Equal(other.Modules[i].Parsed) {
			return false
		}
		if !bytes.Equal(b.Modules[i].Raw, other.Modules[i].Raw) {
			return false
		}
	}
	if (b.Wasm == nil && other.Wasm != nil) || (b.Wasm != nil && other.Wasm == nil) {
		return false
	}

	return bytes.Equal(b.Wasm, other.Wasm)
}

// Copy returns a deep copy of the bundle.
func (b Bundle) Copy() Bundle {

	// Copy data.
	var x interface{} = b.Data

	if err := util.RoundTrip(&x); err != nil {
		panic(err)
	}

	if x != nil {
		b.Data = x.(map[string]interface{})
	}

	// Copy modules.
	for i := range b.Modules {
		bs := make([]byte, len(b.Modules[i].Raw))
		copy(bs, b.Modules[i].Raw)
		b.Modules[i].Raw = bs
		b.Modules[i].Parsed = b.Modules[i].Parsed.Copy()
	}

	// Copy manifest.
	b.Manifest = b.Manifest.Copy()

	return b
}

func (b *Bundle) insertData(key []string, value interface{}) error {
	// Build an object with the full structure for the value
	obj, err := mktree(key, value)
	if err != nil {
		return err
	}

	// Merge the new data in with the current bundle data object
	merged, ok := merge.InterfaceMaps(b.Data, obj)
	if !ok {
		return fmt.Errorf("failed to insert data file from path %s", filepath.Join(key...))
	}

	b.Data = merged

	return nil
}

func (b *Bundle) readData(key []string) *interface{} {

	if len(key) == 0 {
		if len(b.Data) == 0 {
			return nil
		}
		var result interface{} = b.Data
		return &result
	}

	node := b.Data

	for i := 0; i < len(key)-1; i++ {

		child, ok := node[key[i]]
		if !ok {
			return nil
		}

		childObj, ok := child.(map[string]interface{})
		if !ok {
			return nil
		}

		node = childObj
	}

	child, ok := node[key[len(key)-1]]
	if !ok {
		return nil
	}

	return &child
}

func mktree(path []string, value interface{}) (map[string]interface{}, error) {
	if len(path) == 0 {
		// For 0 length path the value is the full tree.
		obj, ok := value.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("root value must be object")
		}
		return obj, nil
	}

	dir := map[string]interface{}{}
	for i := len(path) - 1; i > 0; i-- {
		dir[path[i]] = value
		value = dir
		dir = map[string]interface{}{}
	}
	dir[path[0]] = value

	return dir, nil
}

// Merge accepts a set of bundles and merges them into a single result bundle. If there are
// any conflicts during the merge (e.g., with roots) an error is returned. The result bundle
// will have an empty revision except in the special case where a single bundle is provided
// (and in that case the bundle is just returned unmodified.) Merge currently returns an error
// if multiple bundles are provided and any of those bundles contain wasm modules (because
// wasm module merging is not implemented.)
func Merge(bundles []*Bundle) (*Bundle, error) {

	if len(bundles) == 0 {
		return nil, errors.New("expected at least one bundle")
	}

	if len(bundles) == 1 {
		return bundles[0], nil
	}

	var roots []string
	var result Bundle

	for _, b := range bundles {

		if b.Manifest.Roots == nil {
			return nil, errors.New("bundle manifest not initialized")
		}

		roots = append(roots, *b.Manifest.Roots...)

		if len(b.Wasm) > 0 {
			return nil, errors.New("wasm bundles cannot be merged")
		}

		result.Modules = append(result.Modules, b.Modules...)

		for _, root := range *b.Manifest.Roots {
			key := strings.Split(root, "/")
			if val := b.readData(key); val != nil {
				if err := result.insertData(key, *val); err != nil {
					return nil, err
				}
			}
		}
	}

	result.Manifest.Roots = &roots

	if err := result.Manifest.validateAndInjectDefaults(result); err != nil {
		return nil, err
	}

	return &result, nil
}

// RootPathsOverlap takes in two bundle root paths and returns true if they overlap.
func RootPathsOverlap(pathA string, pathB string) bool {
	a := rootPathSegments(pathA)
	b := rootPathSegments(pathB)
	return rootContains(a, b) || rootContains(b, a)
}

// RootPathsContain takes a set of bundle root paths and returns true if the path is contained.
func RootPathsContain(roots []string, path string) bool {
	segments := rootPathSegments(path)
	for i := range roots {
		if rootContains(rootPathSegments(roots[i]), segments) {
			return true
		}
	}
	return false
}

func rootPathSegments(path string) []string {
	return strings.Split(path, "/")
}

func rootContains(root []string, other []string) bool {

	// A single segment, empty string root always contains the other.
	if len(root) == 1 && root[0] == "" {
		return true
	}

	if len(root) > len(other) {
		return false
	}

	for j := range root {
		if root[j] != other[j] {
			return false
		}
	}

	return true
}

func insertValue(b *Bundle, path string, value interface{}) error {

	// Remove leading / and . characters from the directory path. If the bundle
	// was written with OPA then the paths will contain a leading slash. On the
	// other hand, if the path is empty, filepath.Dir will return '.'.
	// Note: filepath.Dir can return paths with '\' separators, always use
	// filepath.ToSlash to keep them normalized.
	dirpath := strings.TrimLeft(filepath.ToSlash(filepath.Dir(path)), "/.")
	var key []string
	if dirpath != "" {
		key = strings.Split(dirpath, "/")
	}
	if err := b.insertData(key, value); err != nil {
		return errors.Wrapf(err, "bundle load failed on %v", path)
	}
	return nil
}

func dfs(value interface{}, path string, fn func(string, interface{}) (bool, error)) error {
	if stop, err := fn(path, value); err != nil {
		return err
	} else if stop {
		return nil
	}
	obj, ok := value.(map[string]interface{})
	if !ok {
		return nil
	}
	for key := range obj {
		if err := dfs(obj[key], path+"/"+key, fn); err != nil {
			return err
		}
	}
	return nil
}

func modulePathWithPrefix(bundleName string, modulePath string) string {
	// Default prefix is just the bundle name
	prefix := bundleName

	// Bundle names are sometimes just file paths, some of which
	// are full urls (file:///foo/). Parse these and only use the path.
	parsed, err := url.Parse(bundleName)
	if err == nil {
		prefix = filepath.Join(parsed.Host, parsed.Path)
	}

	return filepath.Join(prefix, modulePath)
}
