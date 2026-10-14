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
	"debug/buildinfo"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const closureAppImportPath = "example.com/app"

// newRepo creates a git repository holding files, with nothing committed.
func newRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	// Keep the user's git configuration (signing, hooks, templates) out.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	root := t.TempDir()
	for name, content := range files {
		writeRepoFile(t, root, name, content)
	}
	mustGit(t, root, "init", "-q")
	return root
}

// newClosureRepo creates a repository holding an app module that imports a
// package from a lib module it replaces with a local directory, and returns
// the repository root and the app directory. The app module also holds a
// Terraform module, the kind of working directory the ko provider builds from.
func newClosureRepo(t *testing.T) (string, string) {
	t.Helper()
	root := newRepo(t, map[string]string{
		"unrelated.txt":           "unrelated\n",
		"app/go.mod":              "module example.com/app\n\ngo 1.22\n\nrequire example.com/lib v0.0.0\n\nreplace example.com/lib => ../lib\n",
		"app/main.go":             "package main\n\nimport \"example.com/lib\"\n\nfunc main() { lib.Hello() }\n",
		"app/kodata/greeting.txt": "hello\n",
		"app/iac/main.tf":         "# Terraform that builds the app\n",
		"lib/go.mod":              "module example.com/lib\n\ngo 1.22\n",
		"lib/lib.go":              "package lib\n\nfunc Hello() {}\n",
		"lib/other/other.go":      "package other\n",
	})
	return root, filepath.Join(root, "app")
}

func writeRepoFile(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=ko", "-c", "user.email=ko@example.com", "-c", "commit.gpgSign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitAll(t *testing.T, root, msg string) string {
	t.Helper()
	mustGit(t, root, "add", "-A")
	mustGit(t, root, "commit", "-q", "-m", msg)
	return mustGit(t, root, "rev-parse", "HEAD")
}

func closureTestEnv() []string {
	return append(os.Environ(), "GOWORK=off", "GOPROXY=off", "GOFLAGS=", "CGO_ENABLED=0")
}

// closureFor resolves the app's repository and inputs.
func closureFor(t *testing.T, app string) (repository, inputs) {
	t.Helper()
	dir, err := resolvedDir(app)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := openRepo(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	in, err := listInputs(t.Context(), repo, dir, closureAppImportPath, closureTestEnv(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return repo, in
}

func TestResolveClosureFollowsInputs(t *testing.T) {
	root, app := newClosureRepo(t)
	initial := commitAll(t, root, "initial")

	tests := []struct {
		name string
		file string
		// moves reports whether the change is to an input, so the
		// closure commit moves to it.
		moves bool
	}{
		{name: "file outside the closure", file: "unrelated.txt"},
		{name: "unimported package of a replaced module", file: "lib/other/other.go"},
		{name: "imported package of a replaced module", file: "lib/lib.go", moves: true},
		{name: "kodata", file: "app/kodata/greeting.txt", moves: true},
		{name: "replaced module go.mod", file: "lib/go.mod", moves: true},
		{name: "main package", file: "app/main.go", moves: true},
	}
	want := initial
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(root, tc.file)
			content, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			writeRepoFile(t, root, tc.file, string(content)+"// changed\n")
			commit := commitAll(t, root, "change "+tc.file)
			if tc.moves {
				want = commit
			}

			repo, in := closureFor(t, app)
			got, err := repo.lastCommit(t.Context(), in)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("closure commit: got = %s, want = %s", got, want)
			}
		})
	}
}

func TestClosureOfPackageAtRepositoryRoot(t *testing.T) {
	root := newRepo(t, map[string]string{
		"go.mod":  "module example.com/app\n\ngo 1.22\n",
		"main.go": "package main\n\nfunc main() {}\n",
	})
	commitAll(t, root, "initial")
	writeRepoFile(t, root, "main.go", "package main\n\nfunc main() { println() }\n")
	want := commitAll(t, root, "change main.go")

	repo, in := closureFor(t, root)
	got, err := repo.lastCommit(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("closure commit: got = %s, want = %s", got, want)
	}

	writeRepoFile(t, root, "extra.go", "package main\n")
	if dirty, err := repo.firstChanged(t.Context(), in); err != nil || dirty == "" {
		t.Errorf("firstChanged with an untracked root file: got = %q, %v, want a change", dirty, err)
	}
}

func TestClosureCheckoutBuildsStableStampedBinary(t *testing.T) {
	root, app := newClosureRepo(t)
	initial := commitAll(t, root, "initial")

	build := func() []byte {
		t.Helper()
		// Build from a directory that holds no input, as the ko provider does.
		g := &gobuild{dir: filepath.Join(app, "iac")}
		dir, cleanup, err := g.closureBuildDir(t.Context(), closureAppImportPath, closureTestEnv(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		out := filepath.Join(t.TempDir(), "app")
		cmd := exec.Command(getGoBinary(), "build", "-trimpath", "-o", out, closureAppImportPath)
		cmd.Dir = dir
		cmd.Env = closureTestEnv()
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build: %v: %s", err, output)
		}
		bin, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		info, err := buildinfo.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		settings := map[string]string{}
		for _, s := range info.Settings {
			settings[s.Key] = s.Value
		}
		for key, want := range map[string]string{"vcs": "git", "vcs.revision": initial, "vcs.modified": "false"} {
			if got := settings[key]; got != want {
				t.Errorf("build setting %s: got = %q, want = %q", key, got, want)
			}
		}
		return bin
	}

	before := build()
	writeRepoFile(t, root, "unrelated.txt", "changed\n")
	commitAll(t, root, "unrelated change")
	if after := build(); !bytes.Equal(before, after) {
		t.Error("binary changed after a commit outside its inputs")
	}
}

func TestClosureBuildDirFallsBackOnLocalChanges(t *testing.T) {
	tests := []struct {
		name   string
		change func(t *testing.T, root string)
	}{{
		name:   "untracked file in a package directory",
		change: func(t *testing.T, root string) { writeRepoFile(t, root, "app/extra.go", "package main\n") },
	}, {
		name: "modified input",
		change: func(t *testing.T, root string) {
			writeRepoFile(t, root, "lib/lib.go", "package lib\n\nfunc Hello() { println() }\n")
		},
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, app := newClosureRepo(t)
			commitAll(t, root, "initial")
			tc.change(t, root)

			g := &gobuild{dir: app}
			dir, cleanup, err := g.closureBuildDir(t.Context(), closureAppImportPath, closureTestEnv(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if dir != app {
				t.Errorf("build dir: got = %s, want the working tree %s", dir, app)
			}
		})
	}
}

func TestResolveClosureRejectsShallowClone(t *testing.T) {
	root, _ := newClosureRepo(t)
	commitAll(t, root, "initial")
	writeRepoFile(t, root, "unrelated.txt", "changed\n")
	commitAll(t, root, "second")

	clone := t.TempDir()
	mustGit(t, clone, "clone", "-q", "--depth=1", "file://"+root, ".")

	_, err := openRepo(t.Context(), clone)
	if err == nil || !strings.Contains(err.Error(), "shallow") {
		t.Errorf("openRepo on a shallow clone: got err = %v, want a shallow clone error", err)
	}
}

func TestClosureCheckoutLeavesSourceRepositoryAlone(t *testing.T) {
	root, app := newClosureRepo(t)
	commitAll(t, root, "initial")
	state := func() string {
		return mustGit(t, root, "config", "--list", "--local") + "\n" + mustGit(t, root, "worktree", "list")
	}
	before := state()

	repo, in := closureFor(t, app)
	commit, err := repo.lastCommit(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	dir, cleanup, err := repo.sparseCheckout(t.Context(), commit, in)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()

	if after := state(); after != before {
		t.Errorf("source repository changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("checkout %s still exists after cleanup: %v", dir, err)
	}
}

func TestKodataFilesFollowsSymlinksWithinRepository(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	writeRepoFile(t, outside, "secret.txt", "secret\n")
	writeRepoFile(t, dir, "shared/data.txt", "data\n")
	writeRepoFile(t, dir, "shared/dir/nested.txt", "nested\n")
	writeRepoFile(t, dir, "app/kodata/plain.txt", "plain\n")
	for link, target := range map[string]string{
		"app/kodata/file-link":       "../../shared/data.txt",
		"app/kodata/dir-link":        "../../shared/dir",
		"app/kodata/chain":           "../../shared/hop",
		"shared/hop":                 "data.txt",
		"app/kodata/escape":          outside,
		"app/kodata/escape-relative": "../../../escaped",
	} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	got, err := repository{dir: dir, root: root, fsys: root.FS()}.kodataFiles("app/kodata")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	got = slices.Compact(got)
	// Every symlink on the way counts, and links leaving the repository are
	// listed themselves but never followed.
	want := []string{
		"app/kodata/chain",
		"app/kodata/dir-link",
		"app/kodata/escape",
		"app/kodata/escape-relative",
		"app/kodata/file-link",
		"app/kodata/plain.txt",
		"shared/data.txt",
		"shared/dir/nested.txt",
		"shared/hop",
	}
	if !slices.Equal(got, want) {
		t.Errorf("kodataFiles: got = %v, want = %v", got, want)
	}
}

func TestClosureCommitFromEnv(t *testing.T) {
	tests := []struct {
		value   string
		want    bool
		wantErr bool
	}{
		{value: "", want: false},
		{value: "true", want: true},
		{value: "0", want: false},
		{value: "sometimes", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(closureCommitEnv, tc.value)
			got, err := closureCommitFromEnv()
			if (err != nil) != tc.wantErr {
				t.Fatalf("closureCommitFromEnv: got err = %v, want error = %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("closureCommitFromEnv: got = %v, want = %v", got, tc.want)
			}
		})
	}
}
