package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const identityScript = "assets/system/identity.sh"

// installIdentityScript copies the shipped asset into a temp probe-dir
// layout (probeDir/system/identity.sh, probeDir/programs/) so the script's
// $0-derived path computation lands on a realistic directory structure
// rather than the assets tree. Returns the probe dir and the installed
// script's absolute path.
func installIdentityScript(t *testing.T) (probeDir, scriptPath string) {
	t.Helper()
	probeDir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(probeDir, "programs"), 0755); err != nil {
		t.Fatal(err)
	}
	sysDir := filepath.Join(probeDir, "system")
	if err := os.MkdirAll(sysDir, 0755); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(identityScript)
	if err != nil {
		t.Fatalf("reading asset %s: %v", identityScript, err)
	}
	scriptPath = filepath.Join(sysDir, "identity.sh")
	if err := os.WriteFile(scriptPath, src, 0755); err != nil {
		t.Fatal(err)
	}
	return probeDir, scriptPath
}

func TestIdentityScript_EmitsPromptWithResolvedProgramsDir(t *testing.T) {
	t.Parallel()
	probeDir, scriptPath := installIdentityScript(t)
	out := runScript(t, scriptPath, "", os.Environ())

	if !containsAny(out, probeDirPathForms(t, probeDir, "programs")) {
		t.Errorf("output missing resolved programs dir under %q:\n%s", probeDir, out)
	}

	// The literal "$AUTOPROBE_PROGRAMS_DIR" string must never reach the
	// model unresolved. The post-cornerstone split silently regressed this
	// (single-quoted heredoc dropped variable expansion); this guard keeps
	// the prompt asserting the actual path going forward.
	if strings.Contains(out, "$AUTOPROBE_PROGRAMS_DIR") {
		t.Errorf("output contains unresolved $AUTOPROBE_PROGRAMS_DIR:\n%s", out)
	}
}

func TestIdentityScript_IndependentOfEnvAndCwd(t *testing.T) {
	t.Parallel()
	probeDir, scriptPath := installIdentityScript(t)

	// Set AUTOPROBE_PROGRAMS_DIR to a bogus value and run from an unrelated
	// cwd. The script must ignore both and derive the path from $0.
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
