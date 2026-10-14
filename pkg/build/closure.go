// Copyright 2026 ko Build Authors All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package build

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/tools/go/packages"
)

// closureCommitEnv enables closure-commit builds: ko builds each binary from
// a sparse checkout of the most recent commit that changed any of its inputs.
// Go stamps that commit as vcs.revision, so the binary, and with it the image,
// only changes when its inputs do.
const closureCommitEnv = "KO_CLOSURE_COMMIT"

func closureCommitFromEnv() (bool, error) {
	v := os.Getenv(closureCommitEnv)
	if v == "" {
		return false, nil
	}
	enabled, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("parsing %s=%q: %w", closureCommitEnv, v, err)
	}
	return enabled, nil
}

// closureBuildDir returns the directory to build ip from, and a function to
// call once the build is done.
func (g *gobuild) closureBuildDir(ctx context.Context, ip string, env, flags []string) (string, func(), error) {
	dir, err := resolvedDir(g.dir)
	if err != nil {
		return "", nil, err
	}
	repo, err := openRepo(ctx, dir)
	if err != nil {
		return "", nil, err
	}
	defer repo.Close()
	in, err := listInputs(ctx, repo, dir, ip, env, flags)
	if err != nil {
		return "", nil, err
	}

	// A checkout would drop local changes, so build those from the working tree.
	dirty, err := repo.firstChanged(ctx, in)
	if err != nil {
		return "", nil, err
	}
	if dirty != "" {
		log.Printf("Building %s from the working tree: its inputs have local changes (%s)", ip, dirty)
		return g.dir, func() {}, nil
	}

	commit, err := repo.lastCommit(ctx, in)
	if err != nil {
		return "", nil, err
	}
	checkout, cleanup, err := repo.sparseCheckout(ctx, commit, in)
	if err != nil {
		return "", nil, err
	}
	// The working directory, such as a Terraform module next to the Go code,
	// need not hold any input, so the checkout may lack it.
	rel, _ := repo.rel(dir)
	buildDir := filepath.Join(checkout, filepath.FromSlash(rel))
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		cleanup()
		return "", nil, err
	}
	log.Printf("Building %s from %s, the last commit that changed its inputs", ip, commit[:12])
	return buildDir, cleanup, nil
}

// resolvedDir returns dir as an absolute path with symlinks resolved, the way
// git reports the repository root, so that paths under both compare equal.
func resolvedDir(dir string) (string, error) {
	abs, err := filepath.Abs(cmp.Or(dir, "."))
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// repository is the git repository a binary is built from. Inside it, paths
// are relative to the repository root and in slash form, which is what git
// pathspecs and fsys both take. Files are read through an os.Root, so paths
// taken from module files and symlinks cannot reach outside the repository.
type repository struct {
	dir  string
	root *os.Root
	fsys fs.FS
}

func openRepo(ctx context.Context, dir string) (repository, error) {
	out, err := runGit(ctx, dir, "rev-parse", "--show-toplevel", "--is-shallow-repository")
	if err != nil {
		return repository{}, fmt.Errorf("%s requires building from a git repository: %w", closureCommitEnv, err)
	}
	top, shallow, _ := strings.Cut(strings.TrimSpace(out), "\n")
	if shallow == "true" {
		return repository{}, fmt.Errorf("%s requires full history, but %s is a shallow clone (fetch with --unshallow, or clone with --filter=blob:none instead of --depth)", closureCommitEnv, top)
	}
	root, err := os.OpenRoot(top)
	if err != nil {
		return repository{}, err
	}
	return repository{dir: top, root: root, fsys: root.FS()}, nil
}

func (r repository) Close() error {
	return r.root.Close()
}

// rel converts the absolute path p, as the go command reports it, to a
// repository path, and reports whether p lies within the repository at all.
func (r repository) rel(p string) (string, bool) {
	rel, err := filepath.Rel(r.dir, p)
	if err != nil || !filepath.IsLocal(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// firstChanged returns the first input with local changes, or "" if there
// are none. Untracked and ignored files count: go builds any Go file in a
// package directory, committed or not.
func (r repository) firstChanged(ctx context.Context, in inputs) (string, error) {
	out, err := runGit(ctx, r.dir, slices.Concat([]string{"status", "--porcelain", "--untracked-files=all", "--ignored=matching", "--"}, in.pathspecs())...)
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(out, "\n")
	return strings.TrimSpace(line), nil
}

// lastCommit returns the most recent commit, following first parents from
// HEAD, that changed any of the inputs.
func (r repository) lastCommit(ctx context.Context, in inputs) (string, error) {
	out, err := runGit(ctx, r.dir, slices.Concat([]string{"log", "-1", "--first-parent", "--format=%H", "HEAD", "--"}, in.pathspecs())...)
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(out)
	if commit == "" {
		return "", errors.New("no commit changed the build's inputs")
	}
	return commit, nil
}

// sparseCheckout checks out just the inputs at commit into a temporary
// repository and returns its root, along with a function that removes it.
//
// The checkout borrows objects from the repository through
// objects/info/alternates, so creating it never writes to the repository.
// Because it holds only the inputs, an input missing from the list fails the
// build instead of compiling a stale copy. git status reports the files left
// out as skipped rather than deleted, so Go stamps the commit as clean.
func (r repository) sparseCheckout(ctx context.Context, commit string, in inputs) (string, func(), error) {
	files, err := r.filesAt(ctx, commit, in)
	if err != nil {
		return "", nil, err
	}
	objects, err := runGit(ctx, r.dir, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return "", nil, err
	}

	dir, err := os.MkdirTemp("", "ko-closure-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := initSparseRepo(ctx, dir, strings.TrimSpace(objects), commit, files); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("checking out %s: %w", commit, err)
	}
	return dir, cleanup, nil
}

// filesAt lists the files at commit that are inputs: the files directly in
// each input directory, and the input files that exist at that commit.
func (r repository) filesAt(ctx context.Context, commit string, in inputs) ([]string, error) {
	paths := slices.Clone(in.files)
	for _, d := range in.dirs {
		paths = append(paths, d+"/") // A trailing slash lists the directory's entries.
	}
	out, err := runGit(ctx, r.dir, slices.Concat([]string{"ls-tree", "-z", "--full-tree", commit, "--"}, paths)...)
	if err != nil {
		return nil, err
	}
	var files []string
	for entry := range strings.SplitSeq(out, "\x00") {
		// Each entry is "<mode> <type> <object>\t<path>".
		meta, path, _ := strings.Cut(entry, "\t")
		if fields := strings.Fields(meta); len(fields) == 3 && fields[1] == "blob" {
			files = append(files, path)
		}
	}
	return files, nil
}

func initSparseRepo(ctx context.Context, dir, objects, commit string, files []string) error {
	var patterns strings.Builder
	for _, f := range files {
		fmt.Fprintf(&patterns, "/%s\n", globEscaper.Replace(f))
	}
	if _, err := runGit(ctx, dir, "init", "-q"); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "objects", "info", "alternates"), []byte(objects+"\n"), 0o600); err != nil {
		return err
	}
	if _, err := runGit(ctx, dir, "config", "core.sparseCheckout", "true"); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "info", "sparse-checkout"), []byte(patterns.String()), 0o600); err != nil {
		return err
	}
	if _, err := runGit(ctx, dir, "update-ref", "--no-deref", "HEAD", commit); err != nil {
		return err
	}
	_, err := runGit(ctx, dir, "read-tree", "-mu", "HEAD")
	return err
}

// globEscaper escapes the characters git treats as wildcards in glob
// pathspecs and sparse-checkout patterns.
var globEscaper = strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`)

// inputs are the repository-relative paths, in slash form, that determine a
// binary.
type inputs struct {
	// dirs are the binary's package directories. Every file directly in them
	// counts, regardless of build constraints.
	dirs []string
	// files are embedded files, module and workspace files, and kodata.
	files []string
}

func (in inputs) pathspecs() []string {
	specs := make([]string, 0, len(in.dirs)+len(in.files))
	for _, d := range in.dirs {
		// A package at the repository root has the directory ".", and
		// "./*" would match nothing, so join rather than concatenate.
		specs = append(specs, ":(top,glob)"+path.Join(globEscaper.Replace(d), "*"))
	}
	for _, f := range in.files {
		specs = append(specs, ":(top,literal)"+f)
	}
	return specs
}

// listInputs lists the inputs within repo of the binary at ip, built in dir
// with env and flags.
func listInputs(ctx context.Context, repo repository, dir, ip string, env, flags []string) (inputs, error) {
	main, err := loadPackage(ctx, dir, ip, env, flags)
	if err != nil {
		return inputs{}, err
	}
	dirs, files := map[string]struct{}{}, map[string]struct{}{}
	addFile := func(p string) {
		if rel, ok := repo.rel(p); ok {
			files[rel] = struct{}{}
		}
	}
	packages.Visit([]*packages.Package{main}, nil, func(pkg *packages.Package) {
		if rel, ok := repo.rel(pkg.Dir); ok {
			dirs[rel] = struct{}{}
		}
		for _, f := range pkg.EmbedFiles {
			addFile(f)
		}
		if pkg.Module != nil {
			addFile(pkg.Module.GoMod)
		}
	})

	modFiles, err := repo.moduleFiles(ctx, dir, env)
	if err != nil {
		return inputs{}, err
	}
	// A main package from a dependency module brings its kodata from the
	// module cache, pinned by go.sum.
	var kodata []string
	if mainDir, ok := repo.rel(main.Dir); ok {
		if kodata, err = repo.kodataFiles(path.Join(mainDir, "kodata")); err != nil {
			return inputs{}, err
		}
	}
	for _, f := range slices.Concat(modFiles, kodata) {
		files[f] = struct{}{}
	}
	return inputs{dirs: slices.Sorted(maps.Keys(dirs)), files: slices.Sorted(maps.Keys(files))}, nil
}

// loadPackage loads the main package at ip and its dependencies the way the
// build sees them.
func loadPackage(ctx context.Context, dir, ip string, env, flags []string) (*packages.Package, error) {
	// Flags can carry build tags, which decide what gets imported. Only
	// template them when they use templates: gathering the template data
	// logs git notices that the build logs again.
	if slices.ContainsFunc(flags, func(f string) bool { return strings.Contains(f, "{{") }) {
		var err error
		if flags, err = createBuildArgs(ctx, buildContext{ip: ip, dir: dir, env: env, flags: flags}); err != nil {
			return nil, err
		}
	}
	pkgs, err := packages.Load(&packages.Config{
		Context:    ctx,
		Dir:        dir,
		Env:        env,
		BuildFlags: flags,
		Mode:       packages.NeedName | packages.NeedFiles | packages.NeedEmbedFiles | packages.NeedImports | packages.NeedDeps | packages.NeedModule,
	}, ip)
	if err != nil {
		return nil, err
	}
	if len(pkgs) != 1 {
		return nil, fmt.Errorf("found %d packages for %s, expected 1", len(pkgs), ip)
	}
	var errs []error
	packages.Visit(pkgs, nil, func(pkg *packages.Package) {
		for _, e := range pkg.Errors {
			errs = append(errs, e)
		}
	})
	return pkgs[0], errors.Join(errs...)
}

// moduleFiles lists, relative to the repository, the files that shape the
// module graph: the main module's go.mod and go.sum, go.work and go.work.sum
// and the used modules' go.mod and go.sum in workspace mode, and the go.mod of
// every local replacement, which go reads even when the build imports none of
// its packages. Files outside the repository cannot be inputs.
func (r repository) moduleFiles(ctx context.Context, dir string, env []string) ([]string, error) {
	// The go command decides which module is the main one, and whether, and
	// which, workspace applies.
	out, err := runGo(ctx, dir, env, "env", "GOMOD", "GOWORK")
	if err != nil {
		return nil, err
	}
	// One line per variable; either may be empty.
	mainGoMod, goWork, _ := strings.Cut(strings.TrimSuffix(out, "\n"), "\n")
	var goMods, files []string
	if mainGoMod != "" && mainGoMod != os.DevNull {
		goMods = append(goMods, mainGoMod)
	}
	if goWork != "" && goWork != "off" {
		files = append(files, goWork, goWork+".sum")
		data, ok, err := r.readFile(goWork)
		if err != nil {
			return nil, err
		}
		if ok {
			work, err := modfile.ParseWork(goWork, data, nil)
			if err != nil {
				return nil, err
			}
			workDir := filepath.Dir(goWork)
			for _, use := range work.Use {
				goMods = append(goMods, filepath.Join(resolvePath(workDir, use.Path), "go.mod"))
			}
			files = append(files, localReplacements(workDir, work.Replace)...)
		}
	}

	for _, goMod := range goMods {
		modDir := filepath.Dir(goMod)
		files = append(files, goMod, filepath.Join(modDir, "go.sum"))
		data, ok, err := r.readFile(goMod)
		if err != nil {
			return nil, err
		}
		if ok {
			mod, err := modfile.Parse(goMod, data, nil)
			if err != nil {
				return nil, err
			}
			files = append(files, localReplacements(modDir, mod.Replace)...)
		}
	}

	var rels []string
	for _, f := range files {
		// Not every module has a go.sum.
		if rel, ok := r.rel(f); ok {
			if _, err := fs.Stat(r.fsys, rel); err == nil {
				rels = append(rels, rel)
			}
		}
	}
	return rels, nil
}

// readFile reads the file at the absolute path p, and reports whether p lies
// within the repository at all.
func (r repository) readFile(p string) ([]byte, bool, error) {
	rel, ok := r.rel(p)
	if !ok {
		return nil, false, nil
	}
	data, err := fs.ReadFile(r.fsys, rel)
	return data, true, err
}

// localReplacements returns the go.mod files of the replacements that point
// at directories, resolved against base.
func localReplacements(base string, replacements []*modfile.Replace) []string {
	var goMods []string
	for _, r := range replacements {
		if r.New.Version == "" {
			goMods = append(goMods, filepath.Join(resolvePath(base, r.New.Path), "go.mod"))
		}
	}
	return goMods
}

func resolvePath(base, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

// kodataFiles lists, relative to the repository, the files under the
// repository-relative directory dir, following symlinks the way ko does when it
// copies kodata into the image. Every symlink on the way counts, since changing
// any of them changes the image. Symlinks leaving the repository are not
// inputs, and ko rejects them when it builds the image anyway.
func (r repository) kodataFiles(dir string) ([]string, error) {
	var files []string
	err := fs.WalkDir(r.fsys, dir, func(p string, d fs.DirEntry, err error) error {
		switch {
		case errors.Is(err, fs.ErrNotExist) && p == dir:
			return nil // There is no kodata.
		case err != nil || d.IsDir():
			return err
		}
		target, links, err := r.followLinks(p)
		files = append(files, links...)
		if err != nil || target == "" {
			return err
		}
		info, err := fs.Stat(r.fsys, target)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			files = append(files, target)
			return nil
		}
		nested, err := r.kodataFiles(target)
		files = append(files, nested...)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("listing kodata in %s: %w", dir, err)
	}
	return files, nil
}

// followLinks resolves the repository-relative path p one symlink at a time.
// It returns where p ends up, or "" if a symlink leaves the repository, and
// every symlink on the way.
func (r repository) followLinks(p string) (string, []string, error) {
	var links []string
	for range 40 { // POSIX guarantees at least this many levels.
		info, err := fs.Lstat(r.fsys, p)
		if err != nil || info.Mode()&fs.ModeSymlink == 0 {
			return p, links, err
		}
		links = append(links, p)
		target, err := fs.ReadLink(r.fsys, p)
		if err != nil {
			return "", links, err
		}
		if filepath.IsAbs(target) {
			rel, ok := r.rel(target)
			if !ok {
				return "", links, nil
			}
			p = rel
		} else if p = path.Join(path.Dir(p), filepath.ToSlash(target)); !fs.ValidPath(p) {
			return "", links, nil
		}
	}
	return "", links, fmt.Errorf("%s: too many levels of symbolic links", links[0])
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	/* #nosec G204 -- ko invokes git with arguments it derives from the build. */
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "log.showSignature=false"}, args...)...)
	cmd.Dir = dir
	return output(cmd)
}

func runGo(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	/* #nosec G204 G702 -- ko intentionally invokes the user-configured go toolchain. */
	cmd := exec.CommandContext(ctx, getGoBinary(), args...)
	cmd.Dir = dir
	cmd.Env = env
	return output(cmd)
}

func output(cmd *exec.Cmd) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", filepath.Base(cmd.Path), strings.Join(cmd.Args[1:], " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
