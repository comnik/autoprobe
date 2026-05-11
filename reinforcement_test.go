package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runReinforcement executes a reinforcement script with the given stdin
// JSON, working directory, and AUTOPROBE_PROGRAMS_DIR value. An empty
// programsDir leaves the variable unset; an empty cwd inherits the test
// process's working directory.
func runReinforcement(t *testing.T, script, programsDir, cwd, argsJSON string) string {
	t.Helper()
	abs, err := filepath.Abs(script)
	if err != nil {
		t.Fatalf("resolving %s: %v", script, err)
	}
	cmd := exec.Command(abs)
	cmd.Stdin = strings.NewReader(argsJSON)
	env := os.Environ()
	if programsDir != "" {
		env = append(env, "AUTOPROBE_PROGRAMS_DIR="+programsDir)
	}
	cmd.Env = env
	if cwd != "" {
		cmd.Dir = cwd
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s failed: %v\nstderr: %s", filepath.Base(script), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

const (
	libraryScript = "assets/reinforcement/write/library.sh"
	generalScript = "assets/reinforcement/write/general.sh"

	// Sentinel substrings unique to each reinforcement payload. If the
	// canonical messages drift, these guards force the tests to be
	// updated deliberately.
	libraryMarker = "lexicographically"
	generalMarker = "only persistent memory"
)

func TestLibraryReinforcement_FiresForAbsolutePathInsideProgramsDir(t *testing.T) {
	t.Parallel()
	pd := t.TempDir()
	out := runReinforcement(t, libraryScript, pd, "",
		`{"path":"`+filepath.Join(pd, "foo.sh")+`","content":"x"}`)
	if !strings.Contains(out, libraryMarker) {
		t.Fatalf("expected full library reinforcement, got: %q", out)
	}
}

func TestLibraryReinforcement_FiresForRelativePathResolvingInside(t *testing.T) {
	t.Parallel()
	root, pd := relativePathSetup(t)
	// cwd=root + relative path "programs/foo.sh" should resolve to pd/foo.sh.
	out := runReinforcement(t, libraryScript, pd, root,
		`{"path":"programs/foo.sh","content":"x"}`)
	if !strings.Contains(out, libraryMarker) {
		t.Fatalf("expected library reinforcement for relative path, got: %q", out)
	}
}

func TestLibraryReinforcement_SilentOutsideProgramsDir(t *testing.T) {
	t.Parallel()
	pd := t.TempDir()
	elsewhere := t.TempDir()
	out := runReinforcement(t, libraryScript, pd, "",
		`{"path":"`+filepath.Join(elsewhere, "foo.txt")+`","content":"x"}`)
	if out != "" {
		t.Fatalf("expected silence outside programs dir, got: %q", out)
	}
}

func TestLibraryReinforcement_SilentWhenProgramsDirUnset(t *testing.T) {
	t.Parallel()
	out := runReinforcement(t, libraryScript, "", "",
		`{"path":"/tmp/foo.sh","content":"x"}`)
	if out != "" {
		t.Fatalf("expected silence when AUTOPROBE_PROGRAMS_DIR is unset, got: %q", out)
	}
}

func TestLibraryReinforcement_SilentWhenPathMissing(t *testing.T) {
	t.Parallel()
	pd := t.TempDir()
	out := runReinforcement(t, libraryScript, pd, "", `{"content":"x"}`)
	if out != "" {
		t.Fatalf("expected silence when path is missing, got: %q", out)
	}
}

func TestGeneralReinforcement_FiresForPathOutsideProgramsDir(t *testing.T) {
	t.Parallel()
	pd := t.TempDir()
	elsewhere := t.TempDir()
	out := runReinforcement(t, generalScript, pd, "",
		`{"path":"`+filepath.Join(elsewhere, "foo.txt")+`","content":"x"}`)
	if !strings.Contains(out, generalMarker) {
		t.Fatalf("expected general reinforcement, got: %q", out)
	}
	// The general message should NOT carry the library-only sections.
	if strings.Contains(out, libraryMarker) {
		t.Fatalf("general output unexpectedly includes library-only content: %q", out)
	}
}

func TestGeneralReinforcement_FiresWhenProgramsDirUnset(t *testing.T) {
	t.Parallel()
	// With no programs dir defined, every write is "outside" and should
	// receive the general nudge.
	out := runReinforcement(t, generalScript, "", "",
		`{"path":"/tmp/foo.txt","content":"x"}`)
	if !strings.Contains(out, generalMarker) {
		t.Fatalf("expected general reinforcement when programs dir is unset, got: %q", out)
	}
}

func TestGeneralReinforcement_SilentForAbsolutePathInsideProgramsDir(t *testing.T) {
	t.Parallel()
	pd := t.TempDir()
	out := runReinforcement(t, generalScript, pd, "",
		`{"path":"`+filepath.Join(pd, "foo.sh")+`","content":"x"}`)
	if out != "" {
		t.Fatalf("expected silence for path inside programs dir, got: %q", out)
	}
}

func TestGeneralReinforcement_SilentForRelativePathResolvingInside(t *testing.T) {
	t.Parallel()
	root, pd := relativePathSetup(t)
	out := runReinforcement(t, generalScript, pd, root,
		`{"path":"programs/foo.sh","content":"x"}`)
	if out != "" {
		t.Fatalf("expected silence for relative path resolving inside programs dir, got: %q", out)
	}
}

// relativePathSetup builds a programs dir under a temp root and returns
// both paths canonicalized through filepath.EvalSymlinks. On macOS,
// $TMPDIR commonly resolves through /var → /private/var, so the
// AUTOPROBE_PROGRAMS_DIR env value must match the path the child shell
// sees via getcwd().
func relativePathSetup(t *testing.T) (root, pd string) {
	t.Helper()
	root = t.TempDir()
	pd = filepath.Join(root, "programs")
	if err := os.MkdirAll(pd, 0755); err != nil {
		t.Fatal(err)
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(root): %v", err)
	}
	pdResolved, err := filepath.EvalSymlinks(pd)
	if err != nil {
		t.Fatalf("EvalSymlinks(pd): %v", err)
	}
	return rootResolved, pdResolved
}

func TestGeneralReinforcement_SilentWhenPathMissing(t *testing.T) {
	t.Parallel()
	pd := t.TempDir()
	out := runReinforcement(t, generalScript, pd, "", `{"content":"x"}`)
	if out != "" {
		t.Fatalf("expected silence when path is missing, got: %q", out)
	}
}
