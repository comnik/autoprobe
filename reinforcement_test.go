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
	libraryScript  = "assets/reinforcement/write/library.sh"
	generalScript  = "assets/reinforcement/write/general.sh"
	revisionScript = "assets/reinforcement/revision/general.sh"

	// Sentinel substrings unique to each reinforcement payload. If the
	// canonical messages drift, these guards force the tests to be
	// updated deliberately.
	libraryMarker     = "lexicographically"
	generalMarker     = "only persistent memory"
	revisionMarker    = "[REVISION]"
	revisionInactive  = "demotion list lives at"
	revisionRewrite   = "Improve information density"
	revisionExitCode  = "exit non-zero"
	revisionTailSlot  = "20% exploration slot"
	revisionWriteEdit = "write/edit tools"
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

// installRevisionScript copies the shipped asset into a temp probe-dir
// layout (probeDir/programs, probeDir/reinforcement/revision/general.sh) so
// the script's $0-derived path computation lands on a realistic directory
// structure rather than the assets tree. Returns the probe dir and the
// installed script's absolute path.
func installRevisionScript(t *testing.T) (probeDir, scriptPath string) {
	t.Helper()
	probeDir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(probeDir, "programs"), 0755); err != nil {
		t.Fatal(err)
	}
	revDir := filepath.Join(probeDir, "reinforcement", "revision")
	if err := os.MkdirAll(revDir, 0755); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(revisionScript)
	if err != nil {
		t.Fatalf("reading asset %s: %v", revisionScript, err)
	}
	scriptPath = filepath.Join(revDir, "general.sh")
	if err := os.WriteFile(scriptPath, src, 0755); err != nil {
		t.Fatal(err)
	}
	return probeDir, scriptPath
}

// runScript executes the given absolute script path with no stdin and no
// env overrides (other than inherited os.Environ). Failures are fatal —
// the revision script is expected to succeed; silent failure would mask
// the bug class these tests exist to catch.
func runScript(t *testing.T, scriptPath, cwd string, env []string) string {
	t.Helper()
	cmd := exec.Command(scriptPath)
	cmd.Env = env
	if cwd != "" {
		cmd.Dir = cwd
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s failed: %v\nstderr: %s", filepath.Base(scriptPath), err, stderr.String())
	}
	return stdout.String()
}

// probeDirPathForms returns the candidate path strings under probeDir/sub
// the script could plausibly emit: both the as-given form and the
// EvalSymlinks-canonicalized form. On macOS, /var → /private/var is a
// system symlink; depending on which form $0 was invoked with and whether
// `cd ... && pwd` canonicalizes, either may appear. Asserting "contains
// one of these forms" keeps the test portable.
func probeDirPathForms(t *testing.T, probeDir, sub string) []string {
	t.Helper()
	forms := []string{filepath.Join(probeDir, sub)}
	if resolved, err := filepath.EvalSymlinks(probeDir); err == nil && resolved != probeDir {
		forms = append(forms, filepath.Join(resolved, sub))
	}
	return forms
}

func containsAny(s string, candidates []string) bool {
	for _, c := range candidates {
		if strings.Contains(s, c) {
			return true
		}
	}
	return false
}

func TestRevisionScript_EmitsPromptWithResolvedPaths(t *testing.T) {
	t.Parallel()
	probeDir, scriptPath := installRevisionScript(t)
	out := runScript(t, scriptPath, "", os.Environ())

	// All section anchors must appear so a future refactor that silently
	// drops part of the prompt fails the test rather than the agent.
	for _, want := range []string{revisionMarker, revisionInactive, revisionRewrite, revisionExitCode, revisionTailSlot, revisionWriteEdit} {
		if !strings.Contains(out, want) {
			t.Errorf("revision prompt missing marker %q in output", want)
		}
	}

	if !containsAny(out, probeDirPathForms(t, probeDir, "programs")) {
		t.Errorf("output missing resolved programs dir under %q:\n%s", probeDir, out)
	}
	if !containsAny(out, probeDirPathForms(t, probeDir, "inactive")) {
		t.Errorf("output missing resolved inactive path under %q:\n%s", probeDir, out)
	}

	// Defensive: the literal "$AUTOPROBE_PROGRAMS_DIR" string must never
	// reach the agent unresolved. If it does, the script either expanded
	// the wrong variable or someone reverted to the Go-string version.
	if strings.Contains(out, "$AUTOPROBE_PROGRAMS_DIR") {
		t.Errorf("output contains unresolved $AUTOPROBE_PROGRAMS_DIR:\n%s", out)
	}
}

func TestRevisionScript_IndependentOfEnvAndCwd(t *testing.T) {
	t.Parallel()
	probeDir, scriptPath := installRevisionScript(t)

	// Set AUTOPROBE_PROGRAMS_DIR to a bogus value and run from an unrelated
	// cwd. The script must ignore both and derive paths from $0.
	env := []string{"AUTOPROBE_PROGRAMS_DIR=/should/not/appear/in/output", "PATH=" + os.Getenv("PATH")}
	cwd := t.TempDir()
	out := runScript(t, scriptPath, cwd, env)

	if strings.Contains(out, "/should/not/appear") {
		t.Errorf("script honored $AUTOPROBE_PROGRAMS_DIR instead of deriving from $0:\n%s", out)
	}
	if !containsAny(out, probeDirPathForms(t, probeDir, "programs")) {
		t.Errorf("script did not resolve to the installed probe dir; cwd=%s probeDir=%s output:\n%s", cwd, probeDir, out)
	}
}
