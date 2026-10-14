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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// closureCommitEnv enables closure-commit builds. ko then builds each binary
// from a sparse checkout of the most recent commit that changed any of the
// binary's inputs, instead of from the working tree. Go stamps that commit as
// vcs.revision, so the binary, and with it the image, only changes when its
// inputs do, while the stamp stays a real, clean commit.
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

// closureInputs are the repository-relative paths, in slash form, that
// determine a binary.
type closureInputs struct {
	// dirs are the directories of the binary's packages within the
	// repository. Every file directly in them counts, regardless of build
	// constraints.
	dirs []string
	// files are the embedded files and the module and workspace files.
	files []string
	// kodata are the files ko copies into the image, with symlinks resolved.
	// They select the commit but stay out of the checkout, because ko reads
	// kodata from the working tree.
	kodata []string
}

func (in closureInputs) pathspecs() []string {
	specs := make([]string, 0, len(in.dirs)+len(in.files)+len(in.kodata))
	for _, d := range in.dirs {
		specs = append(specs, ":(top,glob)"+globEscaper.Replace(d)+"/*")
	}
	for _, f := range slices.Concat(in.files, in.kodata) {
		specs = append(specs, ":(top,literal)"+f)
	}
	return specs
}

// globEscaper escapes the characters that git treats as wildcards, both in
// glob pathspecs and in sparse-checkout patterns.
var globEscaper = strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`)

// closure is what a closure-commit build of one binary resolves to.
type closure struct {
	root   string
	dir    string
	commit string
	inputs closureInputs
	// dirty names an input with local changes. A checkout would drop them,
	// so such a binary is built from the working tree instead.
	dirty string
}

// resolveClosure finds the inputs of the binary at importpath ip, built in
// dir with env and flags, and the most recent commit that changed any of them.
func resolveClosure(ctx context.Context, dir, ip string, env, flags []string) (closure, error) {
	if dir == "" {
		dir = "."
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return closure{}, err
	}
	// git reports the repository root with symlinks resolved, so resolve dir
	// too, and run go there, to keep every path comparable to the root.
	if absDir, err = filepath.EvalSymlinks(absDir); err != nil {
		return closure{}, err
	}

	out, err := gitOutput(ctx, absDir, "rev-parse", "--show-toplevel", "--is-shallow-repository")
	if err != nil {
		return closure{}, fmt.Errorf("%s requires building from a git repository: %w", closureCommitEnv, err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		return closure{}, fmt.Errorf("unexpected output from git rev-parse: %q", out)
	}
	root := lines[0]
	if lines[1] == "true" {
		return closure{}, fmt.Errorf("%s requires full history, but %s is a shallow clone (fetch with --unshallow, or clone with --filter=blob:none instead of --depth)", closureCommitEnv, root)
	}

	inputs, err := closureInputsFor(ctx, root, absDir, ip, env, flags)
	if err != nil {
		return closure{}, err
	}
	specs := inputs.pathspecs()
	c := closure{root: root, dir: absDir, inputs: inputs}

	// Untracked and ignored files count: go builds any Go file in a package
	// directory, committed or not.
	out, err = gitOutput(ctx, root, append([]string{"status", "--porcelain", "--untracked-files=all", "--ignored=matching", "--"}, specs...)...)
	if err != nil {
		return closure{}, err
	}
	if line, _, _ := strings.Cut(out, "\n"); line != "" {
		c.dirty = strings.TrimSpace(line)
		return c, nil
	}

	out, err = gitOutput(ctx, root, append([]string{"log", "-1", "--first-parent", "--format=%H", "HEAD", "--"}, specs...)...)
	if err != nil {
		return closure{}, err
	}
	if c.commit = strings.TrimSpace(out); c.commit == "" {
		return closure{}, fmt.Errorf("no commit changes the inputs of %s", ip)
	}
	return c, nil
}

// closureInputsFor lists the inputs of the binary at ip within root.
func closureInputsFor(ctx context.Context, root, dir, ip string, env, flags []string) (closureInputs, error) {
	var in closureInputs
	addDir := func(p string) error {
		rel, ok, err := relativeTo(root, p)
		if ok {
			in.dirs = append(in.dirs, rel)
		}
		return err
	}
	addFile := func(p string) error {
		rel, ok, err := relativeTo(root, p)
		if ok {
			in.files = append(in.files, rel)
		}
		return err
	}

	// Flags can carry build tags, which decide what gets imported, so go list
	// needs them as the build sees them. Only template them when they use
	// templates: collecting the template data logs git notices the build
	// itself logs again.
	args := flags
	if slices.ContainsFunc(flags, func(f string) bool { return strings.Contains(f, "{{") }) {
		var err error
		if args, err = createBuildArgs(ctx, buildContext{ip: ip, dir: dir, env: env, flags: flags}); err != nil {
			return closureInputs{}, err
		}
	}
	out, err := goOutput(ctx, dir, env, slices.Concat([]string{"list", "-deps", "-json=ImportPath,Dir,Standard,EmbedFiles,Module"}, args, []string{ip})...)
	if err != nil {
		return closureInputs{}, err
	}
	mainDir := ""
	for dec := json.NewDecoder(strings.NewReader(out)); ; {
		var pkg struct {
			ImportPath string
			Dir        string
			Standard   bool
			EmbedFiles []string
			Module     *struct{ GoMod string }
		}
		if err := dec.Decode(&pkg); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return closureInputs{}, fmt.Errorf("decoding go list output: %w", err)
		}
		if pkg.Standard {
			continue
		}
		if pkg.ImportPath == ip {
			mainDir = pkg.Dir
		}
		if err := addDir(pkg.Dir); err != nil {
			return closureInputs{}, err
		}
		for _, f := range pkg.EmbedFiles {
			if err := addFile(filepath.Join(pkg.Dir, f)); err != nil {
				return closureInputs{}, err
			}
		}
		if pkg.Module != nil && pkg.Module.GoMod != "" {
			if err := addFile(pkg.Module.GoMod); err != nil {
				return closureInputs{}, err
			}
		}
	}
	if mainDir == "" {
		return closureInputs{}, fmt.Errorf("go list did not report %s", ip)
	}

	modFiles, err := moduleFiles(ctx, dir, env)
	if err != nil {
		return closureInputs{}, err
	}
	for _, f := range modFiles {
		if err := addFile(f); err != nil {
			return closureInputs{}, err
		}
	}

	kodata, err := kodataFiles(root, filepath.Join(mainDir, "kodata"))
	if err != nil {
		return closureInputs{}, err
	}
	in.kodata = kodata

	slices.Sort(in.dirs)
	in.dirs = slices.Compact(in.dirs)
	slices.Sort(in.files)
	in.files = slices.Compact(in.files)
	return in, nil
}

// moduleFiles lists the files that shape the module graph: the main module's
// go.mod and go.sum, the workspace files in workspace mode, and the go.mod of
// every local replacement. Go reads a replacement's go.mod even when the
// build imports none of its packages.
func moduleFiles(ctx context.Context, dir string, env []string) ([]string, error) {
	out, err := goOutput(ctx, dir, env, "env", "-json", "GOMOD", "GOWORK")
	if err != nil {
		return nil, err
	}
	var goEnv struct{ GOMOD, GOWORK string }
	if err := json.Unmarshal([]byte(out), &goEnv); err != nil {
		return nil, fmt.Errorf("decoding go env output: %w", err)
	}

	var files, goMods []string
	if goEnv.GOMOD != "" && goEnv.GOMOD != os.DevNull {
		goMods = append(goMods, goEnv.GOMOD)
		files = append(files, goEnv.GOMOD, filepath.Join(filepath.Dir(goEnv.GOMOD), "go.sum"))
	}
	if goEnv.GOWORK != "" && goEnv.GOWORK != "off" {
		files = append(files, goEnv.GOWORK, goEnv.GOWORK+".sum")
		out, err := goOutput(ctx, dir, env, "work", "edit", "-json", goEnv.GOWORK)
		if err != nil {
			return nil, err
		}
		var work struct {
			Use     []struct{ DiskPath string }
			Replace []moduleReplace
		}
		if err := json.Unmarshal([]byte(out), &work); err != nil {
			return nil, fmt.Errorf("decoding go work edit output: %w", err)
		}
		workDir := filepath.Dir(goEnv.GOWORK)
		for _, use := range work.Use {
			modDir := resolvePath(workDir, use.DiskPath)
			goMods = append(goMods, filepath.Join(modDir, "go.mod"))
			files = append(files, filepath.Join(modDir, "go.mod"), filepath.Join(modDir, "go.sum"))
		}
		files = append(files, localReplacements(workDir, work.Replace)...)
	}

	for _, goMod := range goMods {
		out, err := goOutput(ctx, dir, env, "mod", "edit", "-json", goMod)
		if err != nil {
			return nil, err
		}
		var mod struct{ Replace []moduleReplace }
		if err := json.Unmarshal([]byte(out), &mod); err != nil {
			return nil, fmt.Errorf("decoding go mod edit output: %w", err)
		}
		files = append(files, localReplacements(filepath.Dir(goMod), mod.Replace)...)
	}

	// Not every module has a go.sum.
	return slices.DeleteFunc(files, func(f string) bool {
		_, err := os.Stat(f)
		return err != nil
	}), nil
}

type moduleReplace struct {
	New struct{ Path, Version string }
}

// localReplacements returns the go.mod files of the replacements that point at
// directories, resolved against base.
func localReplacements(base string, replaces []moduleReplace) []string {
	var files []string
	for _, r := range replaces {
		if r.New.Version == "" {
			files = append(files, filepath.Join(resolvePath(base, r.New.Path), "go.mod"))
		}
	}
	return files
}

func resolvePath(base, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

// kodataFiles lists the files under kodataDir within root, following symlinks
// the way ko does when it copies kodata into the image. It lists both a
// symlink and its target, since changing either changes the image.
func kodataFiles(root, kodataDir string) ([]string, error) {
	var files []string
	add := func(p string) error {
		rel, ok, err := relativeTo(root, p)
		if ok {
			files = append(files, rel)
		}
		return err
	}
	visited := map[string]struct{}{}
	var walk func(dir string) error
	walk = func(dir string) error {
		if _, ok := visited[dir]; ok {
			return nil
		}
		visited[dir] = struct{}{}
		return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			switch {
			case errors.Is(err, fs.ErrNotExist) && p == kodataDir:
				return nil // The binary has no kodata.
			case err != nil || d.IsDir():
				return err
			}
			if err := add(p); err != nil || d.Type()&fs.ModeSymlink == 0 {
				return err
			}
			target, err := filepath.EvalSymlinks(p)
			if err != nil {
				return err
			}
			info, err := os.Stat(target)
			if err != nil {
				return err
			}
			if info.IsDir() {
				return walk(target)
			}
			return add(target)
		})
	}
	if err := walk(kodataDir); err != nil {
		return nil, fmt.Errorf("listing kodata in %s: %w", kodataDir, err)
	}
	slices.Sort(files)
	return slices.Compact(files), nil
}

// relativeTo returns p relative to root in slash form, and whether p lies
// within root at all. Paths outside the repository, such as the module cache,
// are not inputs git can track.
func relativeTo(root, p string) (string, bool, error) {
	if !withinRoot(p, root) {
		return "", false, nil
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return "", false, err
	}
	return filepath.ToSlash(rel), true, nil
}

// checkout materializes the inputs at the closure commit in a temporary
// repository and returns the directory to build from, along with a function
// that removes the checkout.
//
// The checkout is its own repository that borrows objects from the source
// repository through objects/info/alternates, so preparing it never writes to
// the source repository. It is sparse: it holds only the inputs the go
// command reads, so an input missing from the closure fails the build instead
// of compiling a stale copy. git status reports the files outside the sparse
// set as skipped rather than modified, so Go stamps the commit as clean.
func (c closure) checkout(ctx context.Context) (string, func(), error) {
	out, err := gitOutput(ctx, c.root, append([]string{"ls-tree", "-z", "--full-tree", c.commit, "--"}, c.sparsePaths()...)...)
	if err != nil {
		return "", nil, err
	}
	var patterns strings.Builder
	for entry := range strings.SplitSeq(out, "\x00") {
		meta, p, ok := strings.Cut(entry, "\t")
		if fields := strings.Fields(meta); ok && len(fields) == 3 && fields[1] == "blob" {
			fmt.Fprintf(&patterns, "/%s\n", globEscaper.Replace(p))
		}
	}

	commonDir, err := gitOutput(ctx, c.root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", nil, err
	}

	tmp, err := os.MkdirTemp("", "ko-closure-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	steps := []func() error{
		func() error { _, err := gitOutput(ctx, tmp, "init", "-q"); return err },
		func() error {
			return os.WriteFile(filepath.Join(tmp, ".git", "objects", "info", "alternates"), []byte(filepath.Join(strings.TrimSpace(commonDir), "objects")+"\n"), 0o600)
		},
		func() error { _, err := gitOutput(ctx, tmp, "config", "core.sparseCheckout", "true"); return err },
		func() error {
			return os.WriteFile(filepath.Join(tmp, ".git", "info", "sparse-checkout"), []byte(patterns.String()), 0o600)
		},
		func() error { _, err := gitOutput(ctx, tmp, "update-ref", "--no-deref", "HEAD", c.commit); return err },
		func() error { _, err := gitOutput(ctx, tmp, "read-tree", "-mu", "HEAD"); return err },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("checking out %s: %w", c.commit, err)
		}
	}

	rel, err := filepath.Rel(c.root, c.dir)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return filepath.Join(tmp, rel), cleanup, nil
}

// sparsePaths lists the inputs the go command reads, as git ls-tree paths: a
// trailing slash lists a directory's entries without recursing.
func (c closure) sparsePaths() []string {
	paths := make([]string, 0, len(c.inputs.dirs)+len(c.inputs.files))
	for _, d := range c.inputs.dirs {
		paths = append(paths, d+"/")
	}
	return append(paths, c.inputs.files...)
}

// closureBuildDir returns the directory to build ip from when closure-commit
// builds are enabled, and a function to call once the build is done.
func (g *gobuild) closureBuildDir(ctx context.Context, ip string, env, flags []string) (string, func(), error) {
	c, err := resolveClosure(ctx, g.dir, ip, env, flags)
	if err != nil {
		return "", nil, err
	}
	if c.dirty != "" {
		log.Printf("Building %s from the working tree: its inputs have local changes (%s)", ip, c.dirty)
		return g.dir, func() {}, nil
	}
	dir, cleanup, err := c.checkout(ctx)
	if err != nil {
		return "", nil, err
	}
	log.Printf("Building %s from %s, the last commit that changed its inputs", ip, c.commit[:12])
	return dir, cleanup, nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	/* #nosec G204 -- ko invokes git with arguments it derives from the build. */
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "log.showSignature=false"}, args...)...)
	cmd.Dir = dir
	return runOutput(cmd)
}

func goOutput(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	/* #nosec G204 G702 -- ko intentionally invokes the user-configured go toolchain. */
	cmd := exec.CommandContext(ctx, getGoBinary(), args...)
	cmd.Dir = dir
	cmd.Env = env
	return runOutput(cmd)
}

func runOutput(cmd *exec.Cmd) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", filepath.Base(cmd.Path), strings.Join(cmd.Args[1:], " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
