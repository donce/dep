package vsolver

import (
	"bytes"
	"fmt"
	"go/build"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/scanner"
)

var osList []string
var archList []string
var stdlib = make(map[string]struct{})

const stdlibPkgs string = "archive archive/tar archive/zip bufio builtin bytes compress compress/bzip2 compress/flate compress/gzip compress/lzw compress/zlib container container/heap container/list container/ring context crypto crypto/aes crypto/cipher crypto/des crypto/dsa crypto/ecdsa crypto/elliptic crypto/hmac crypto/md5 crypto/rand crypto/rc4 crypto/rsa crypto/sha1 crypto/sha256 crypto/sha512 crypto/subtle crypto/tls crypto/x509 crypto/x509/pkix database database/sql database/sql/driver debug debug/dwarf debug/elf debug/gosym debug/macho debug/pe debug/plan9obj encoding encoding/ascii85 encoding/asn1 encoding/base32 encoding/base64 encoding/binary encoding/csv encoding/gob encoding/hex encoding/json encoding/pem encoding/xml errors expvar flag fmt go go/ast go/build go/constant go/doc go/format go/importer go/parser go/printer go/scanner go/token go/types hash hash/adler32 hash/crc32 hash/crc64 hash/fnv html html/template image image/color image/color/palette image/draw image/gif image/jpeg image/png index index/suffixarray io io/ioutil log log/syslog math math/big math/cmplx math/rand mime mime/multipart mime/quotedprintable net net/http net/http/cgi net/http/cookiejar net/http/fcgi net/http/httptest net/http/httputil net/http/pprof net/mail net/rpc net/rpc/jsonrpc net/smtp net/textproto net/url os os/exec os/signal os/user path path/filepath reflect regexp regexp/syntax runtime runtime/cgo runtime/debug runtime/msan runtime/pprof runtime/race runtime/trace sort strconv strings sync sync/atomic syscall testing testing/iotest testing/quick text text/scanner text/tabwriter text/template text/template/parse time unicode unicode/utf16 unicode/utf8 unsafe"

func init() {
	// The supported systems are listed in
	// https://github.com/golang/go/blob/master/src/go/build/syslist.go
	// The lists are not exported so we need to duplicate them here.
	osListString := "android darwin dragonfly freebsd linux nacl netbsd openbsd plan9 solaris windows"
	osList = strings.Split(osListString, " ")

	archListString := "386 amd64 amd64p32 arm armbe arm64 arm64be ppc64 ppc64le mips mipsle mips64 mips64le mips64p32 mips64p32le ppc s390 s390x sparc sparc64"
	archList = strings.Split(archListString, " ")

	for _, pkg := range strings.Split(stdlibPkgs, " ") {
		stdlib[pkg] = struct{}{}
	}
}

// listPackages lists info for all packages at or below the provided fileRoot,
// optionally folding in data from test files as well.
//
// Directories without any valid Go files are excluded. Directories with
// multiple packages are excluded.
//
// The importRoot parameter is prepended to the relative path when determining
// the import path for each package. The obvious case is for something typical,
// like:
//
// fileRoot = /home/user/go/src/github.com/foo/bar
// importRoot = github.com/foo/bar
//
// Where the fileRoot and importRoot align. However, if you provide:
//
// fileRoot = /home/user/workspace/path/to/repo
// importRoot = github.com/foo/bar
//
// then the root package at path/to/repo will be ascribed import path
// "github.com/foo/bar", and its subpackage "baz" will be
// "github.com/foo/bar/baz".
//
// A PackageTree is returned, which contains the ImportRoot and map of import path
// to PackageOrErr - each path under the root that exists will have either a
// Package, or an error describing why the package is not valid.
func listPackages(fileRoot, importRoot string) (PackageTree, error) {
	// Set up a build.ctx for parsing
	ctx := build.Default
	ctx.GOROOT = ""
	//ctx.GOPATH = strings.TrimSuffix(parent, "/src")
	ctx.GOPATH = ""
	ctx.UseAllFiles = true

	// basedir is the real root of the filesystem tree we're going to walk.
	// This is generally, though not necessarily, a repo root.
	//basedir := filepath.Join(parent, importRoot)
	// filepath.Dir strips off the last element to get its containing dir, which
	// is what we need to prefix the paths in the walkFn in order to get the
	// full import path.
	//impPrfx := filepath.Dir(importRoot)

	//frslash := ensureTrailingSlash(fileRoot)
	//pretty.Printf("parent:\t\t%s\n", parent)
	//pretty.Printf("frslash:\t%s\n", frslash)
	//pretty.Printf("basedir:\t%s\n", basedir)
	//pretty.Printf("importRoot:\t%s\n", importRoot)
	//pretty.Printf("impPrfx:\t%s\n", impPrfx)
	//pretty.Println(parent, importRoot, impPrfx, basedir)
	//pretty.Println(ctx)

	ptree := PackageTree{
		ImportRoot: importRoot,
		Packages:   make(map[string]PackageOrErr),
	}

	// mkfilter returns two funcs that can be injected into a
	// build.Context, letting us filter the results into an "in" and "out" set.
	mkfilter := func(files map[string]struct{}) (in, out func(dir string) (fi []os.FileInfo, err error)) {
		in = func(dir string) (fi []os.FileInfo, err error) {
			all, err := ioutil.ReadDir(dir)
			if err != nil {
				return nil, err
			}

			for _, f := range all {
				if _, exists := files[f.Name()]; exists {
					fi = append(fi, f)
				}
			}
			return fi, nil
		}

		out = func(dir string) (fi []os.FileInfo, err error) {
			all, err := ioutil.ReadDir(dir)
			if err != nil {
				return nil, err
			}

			for _, f := range all {
				if _, exists := files[f.Name()]; !exists {
					fi = append(fi, f)
				}
			}
			return fi, nil
		}

		return
	}

	// helper func to create a Package from a *build.Package
	happy := func(importPath string, p *build.Package) Package {
		// Happy path - simple parsing worked
		pkg := Package{
			ImportPath:  importPath,
			CommentPath: p.ImportComment,
			Name:        p.Name,
			Imports:     p.Imports,
			TestImports: dedupeStrings(p.TestImports, p.XTestImports),
		}

		return pkg
	}

	err := filepath.Walk(fileRoot, func(path string, fi os.FileInfo, err error) error {
		if err != nil && err != filepath.SkipDir {
			return err
		}
		if !fi.IsDir() {
			return nil
		}

		// Skip a few types of dirs
		if !localSrcDir(fi) {
			return filepath.SkipDir
		}

		// Compute the import path. Run the result through ToSlash(), so that windows
		// paths are normalized to Unix separators, as import paths are expected
		// to be.
		ip := filepath.ToSlash(filepath.Join(importRoot, strings.TrimPrefix(path, fileRoot)))
		//pretty.Printf("path:\t\t%s\n", path)
		//pretty.Printf("ip:\t\t%s\n", ip)

		// Find all the imports, across all os/arch combos
		p, err := ctx.ImportDir(path, analysisImportMode())
		var pkg Package
		if err == nil {
			pkg = happy(ip, p)
		} else {
			//pretty.Println(p, err)
			switch terr := err.(type) {
			case *build.NoGoError:
				ptree.Packages[ip] = PackageOrErr{
					Err: err,
				}
				return nil
			case *build.MultiplePackageError:
				// Set this up preemptively, so we can easily just return out if
				// something goes wrong. Otherwise, it'll get transparently
				// overwritten later.
				ptree.Packages[ip] = PackageOrErr{
					Err: err,
				}

				// For now, we're punting entirely on dealing with os/arch
				// combinations. That will be a more significant refactor.
				//
				// However, there is one case we want to allow here - a single
				// file, with "+build ignore", that's a main package. (Ignore is
				// just a convention, but for now it's good enough to just check
				// that.) This is a fairly common way to make a more
				// sophisticated build system than a Makefile allows, so we want
				// to support that case. So, transparently lump the deps
				// together.
				mains := make(map[string]struct{})
				for k, pkgname := range terr.Packages {
					if pkgname == "main" {
						tags, err2 := readFileBuildTags(filepath.Join(path, terr.Files[k]))
						if err2 != nil {
							return nil
						}

						var hasignore bool
						for _, t := range tags {
							if t == "ignore" {
								hasignore = true
								break
							}
						}
						if !hasignore {
							// No ignore tag found - bail out
							return nil
						}
						mains[terr.Files[k]] = struct{}{}
					}
				}
				// Make filtering funcs that will let us look only at the main
				// files, and exclude the main files; inf and outf, respectively
				inf, outf := mkfilter(mains)

				// outf first; if there's another err there, we bail out with a
				// return
				ctx.ReadDir = outf
				po, err2 := ctx.ImportDir(path, analysisImportMode())
				if err2 != nil {
					return nil
				}
				ctx.ReadDir = inf
				pi, err2 := ctx.ImportDir(path, analysisImportMode())
				if err2 != nil {
					return nil
				}
				ctx.ReadDir = nil

				// Use the other files as baseline, they're the main stuff
				pkg = happy(ip, po)
				mpkg := happy(ip, pi)
				pkg.Imports = dedupeStrings(pkg.Imports, mpkg.Imports)
				pkg.TestImports = dedupeStrings(pkg.TestImports, mpkg.TestImports)
			default:
				return err
			}
		}

		ptree.Packages[ip] = PackageOrErr{
			P: pkg,
		}

		return nil
	})

	if err != nil {
		return PackageTree{}, err
	}

	return ptree, nil
}

type wm struct {
	ex map[string]struct{}
	in map[string]struct{}
}

// wmToReach takes an externalReach()-style workmap and transitively walks all
// internal imports until they reach an external path or terminate, then
// translates the results into a slice of external imports for each internal
// pkg.
//
// The basedir string, with a trailing slash ensured, will be stripped from the
// keys of the returned map.
func wmToReach(workmap map[string]wm, basedir string) (rm map[string][]string, err error) {
	// Just brute-force through the workmap, repeating until we make no
	// progress, either because no packages have any unresolved internal
	// packages left (in which case we're done), or because some packages can't
	// find something in the 'in' list (which shouldn't be possible)
	//
	// This implementation is hilariously inefficient in pure computational
	// complexity terms - worst case is some flavor of polynomial, versus O(n)
	// for the filesystem scan done in externalReach(). However, the coefficient
	// for filesystem access is so much larger than for memory twiddling that it
	// would probably take an absurdly large and snaky project to ever have that
	// worst-case polynomial growth supercede (or even become comparable to) the
	// linear side.
	//
	// But, if that day comes, we can improve this algorithm.
	rm = make(map[string][]string)
	var complete bool
	for !complete {
		var progress bool
		complete = true

		for pkg, w := range workmap {
			if len(w.in) == 0 {
				continue
			}
			complete = false
			// Each pass should always empty the original in list, but there
			// could be more in lists inherited from the other package
			// (transitive internal deps)
			for in := range w.in {
				if w2, exists := workmap[in]; !exists {
					return nil, fmt.Errorf("Should be impossible: %s depends on %s, but %s not in workmap", pkg, in, in)
				} else {
					progress = true
					delete(w.in, in)

					for i := range w2.ex {
						w.ex[i] = struct{}{}
					}
					for i := range w2.in {
						w.in[i] = struct{}{}
					}
				}
			}
		}

		if !complete && !progress {
			// Can't conceive of a way that we'd hit this, but this guards
			// against infinite loop
			panic("unreachable")
		}
	}

	// finally, transform to slice for return
	rm = make(map[string][]string)
	// ensure we have a version of the basedir w/trailing slash, for stripping
	rt := strings.TrimSuffix(basedir, string(os.PathSeparator)) + string(os.PathSeparator)

	for pkg, w := range workmap {
		edeps := make([]string, len(w.ex))
		k := 0
		for opkg := range w.ex {
			edeps[k] = opkg
			k++
		}

		rm[strings.TrimPrefix(pkg, rt)] = edeps
	}

	return rm, nil
}

func localSrcDir(fi os.FileInfo) bool {
	// Ignore _foo and .foo, and testdata
	name := fi.Name()
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" {
		return false
	}

	// Ignore dirs that are expressly intended for non-project source
	switch name {
	case "vendor", "Godeps":
		return false
	default:
		return true
	}
}

func readBuildTags(p string) ([]string, error) {
	_, err := os.Stat(p)
	if err != nil {
		return []string{}, err
	}

	d, err := os.Open(p)
	if err != nil {
		return []string{}, err
	}

	objects, err := d.Readdir(-1)
	if err != nil {
		return []string{}, err
	}

	var tags []string
	for _, obj := range objects {

		// only process Go files
		if strings.HasSuffix(obj.Name(), ".go") {
			fp := filepath.Join(p, obj.Name())

			co, err := readGoContents(fp)
			if err != nil {
				return []string{}, err
			}

			// Only look at places where we had a code comment.
			if len(co) > 0 {
				t := findTags(co)
				for _, tg := range t {
					found := false
					for _, tt := range tags {
						if tt == tg {
							found = true
						}
					}
					if !found {
						tags = append(tags, tg)
					}
				}
			}
		}
	}

	return tags, nil
}

func readFileBuildTags(fp string) ([]string, error) {
	co, err := readGoContents(fp)
	if err != nil {
		return []string{}, err
	}

	var tags []string
	// Only look at places where we had a code comment.
	if len(co) > 0 {
		t := findTags(co)
		for _, tg := range t {
			found := false
			for _, tt := range tags {
				if tt == tg {
					found = true
				}
			}
			if !found {
				tags = append(tags, tg)
			}
		}
	}

	return tags, nil
}

// Read contents of a Go file up to the package declaration. This can be used
// to find the the build tags.
func readGoContents(fp string) ([]byte, error) {
	f, err := os.Open(fp)
	defer f.Close()
	if err != nil {
		return []byte{}, err
	}

	var s scanner.Scanner
	s.Init(f)
	var tok rune
	var pos scanner.Position
	for tok != scanner.EOF {
		tok = s.Scan()

		// Getting the token text will skip comments by default.
		tt := s.TokenText()
		// build tags will not be after the package declaration.
		if tt == "package" {
			pos = s.Position
			break
		}
	}

	var buf bytes.Buffer
	f.Seek(0, 0)
	_, err = io.CopyN(&buf, f, int64(pos.Offset))
	if err != nil {
		return []byte{}, err
	}

	return buf.Bytes(), nil
}

// From a byte slice of a Go file find the tags.
func findTags(co []byte) []string {
	p := co
	var tgs []string
	for len(p) > 0 {
		line := p
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			line, p = line[:i], p[i+1:]
		} else {
			p = p[len(p):]
		}
		line = bytes.TrimSpace(line)
		// Only look at comment lines that are well formed in the Go style
		if bytes.HasPrefix(line, []byte("//")) {
			line = bytes.TrimSpace(line[len([]byte("//")):])
			if len(line) > 0 && line[0] == '+' {
				f := strings.Fields(string(line))

				// We've found a +build tag line.
				if f[0] == "+build" {
					for _, tg := range f[1:] {
						tgs = append(tgs, tg)
					}
				}
			}
		}
	}

	return tgs
}

// Get an OS value that's not the one passed in.
func getOsValue(n string) string {
	for _, o := range osList {
		if o != n {
			return o
		}
	}

	return n
}

func isSupportedOs(n string) bool {
	for _, o := range osList {
		if o == n {
			return true
		}
	}

	return false
}

// Get an Arch value that's not the one passed in.
func getArchValue(n string) string {
	for _, o := range archList {
		if o != n {
			return o
		}
	}

	return n
}

func isSupportedArch(n string) bool {
	for _, o := range archList {
		if o == n {
			return true
		}
	}

	return false
}

func ensureTrailingSlash(s string) string {
	return strings.TrimSuffix(s, string(os.PathSeparator)) + string(os.PathSeparator)
}

// helper func to merge, dedupe, and sort strings
func dedupeStrings(s1, s2 []string) (r []string) {
	dedupe := make(map[string]bool)

	if len(s1) > 0 && len(s2) > 0 {
		for _, i := range s1 {
			dedupe[i] = true
		}
		for _, i := range s2 {
			dedupe[i] = true
		}

		for i := range dedupe {
			r = append(r, i)
		}
		// And then re-sort them
		sort.Strings(r)
	} else if len(s1) > 0 {
		r = s1
	} else if len(s2) > 0 {
		r = s2
	}

	return
}

type PackageTree struct {
	ImportRoot string
	Packages   map[string]PackageOrErr
}

type PackageOrErr struct {
	P   Package
	Err error
}

// ExternalReach looks through a PackageTree and computes the list of external
// dependencies (not under the tree at its designated import root) that are
// imported by packages in the tree.
//
// main indicates whether (true) or not (false) to include main packages in the
// analysis. main packages should generally be excluded when analyzing the
// non-root dependency, as they inherently can't be imported.
//
// tests indicates whether (true) or not (false) to include imports from test
// files in packages when computing the reach map.
func (t PackageTree) ExternalReach(main, tests bool) (map[string][]string, error) {
	var someerrs bool

	// world's simplest adjacency list
	workmap := make(map[string]wm)

	var imps []string
	for ip, perr := range t.Packages {
		if perr.Err != nil {
			someerrs = true
			continue
		}
		p := perr.P
		// Skip main packages, unless param says otherwise
		if p.Name == "main" && !main {
			continue
		}

		imps = imps[:0]
		imps = p.Imports
		if tests {
			imps = dedupeStrings(imps, p.TestImports)
		}

		w := wm{
			ex: make(map[string]struct{}),
			in: make(map[string]struct{}),
		}

		for _, imp := range imps {
			if !checkPrefixSlash(filepath.Clean(imp), t.ImportRoot) {
				w.ex[imp] = struct{}{}
			} else {
				if w2, seen := workmap[imp]; seen {
					for i := range w2.ex {
						w.ex[i] = struct{}{}
					}
					for i := range w2.in {
						w.in[i] = struct{}{}
					}
				} else {
					w.in[imp] = struct{}{}
				}
			}
		}

		workmap[ip] = w
	}

	if len(workmap) == 0 {
		if someerrs {
			// TODO proper errs
			return nil, fmt.Errorf("No packages without errors in %s", t.ImportRoot)
		}
		return nil, nil
	}

	//return wmToReach(workmap, t.ImportRoot)
	return wmToReach(workmap, "") // TODO this passes tests, but doesn't seem right
}

func (t PackageTree) ListExternalImports(main, tests bool) ([]string, error) {
	var someerrs bool
	exm := make(map[string]struct{})

	var imps []string
	for _, perr := range t.Packages {
		if perr.Err != nil {
			someerrs = true
			continue
		}

		p := perr.P
		// Skip main packages, unless param says otherwise
		if p.Name == "main" && !main {
			continue
		}

		imps = imps[:0]
		imps = p.Imports
		if tests {
			imps = dedupeStrings(imps, p.TestImports)
		}

		for _, imp := range imps {
			if !checkPrefixSlash(filepath.Clean(imp), t.ImportRoot) {
				exm[imp] = struct{}{}
			}
		}
	}

	if len(exm) == 0 {
		if someerrs {
			// TODO proper errs
			return nil, fmt.Errorf("No packages without errors in %s", t.ImportRoot)
		}
		return nil, nil
	}

	ex := make([]string, len(exm))
	k := 0
	for p := range exm {
		ex[k] = p
		k++
	}

	return ex, nil
}

// checkPrefixSlash checks to see if the prefix is a prefix of the string as-is,
// and that it is either equal OR the prefix + / is still a prefix.
func checkPrefixSlash(s, prefix string) bool {
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	return s == prefix || strings.HasPrefix(s, ensureTrailingSlash(prefix))
}
