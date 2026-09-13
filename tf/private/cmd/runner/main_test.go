package main_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var runnerBinPath = flag.String("runner", "", "path to the runner binary under test")

func runnerBin(t *testing.T) string {
	t.Helper()
	if *runnerBinPath == "" {
		t.Skip("runner binary path not set; pass -runner=<path> or run via bazel test")
	}
	if filepath.IsAbs(*runnerBinPath) {
		return *runnerBinPath
	}
	// Under bazel test, the flag is an rlocation path; resolve it against the
	// runfiles directory so exec.Command receives an absolute path.
	if dir := os.Getenv("RUNFILES_DIR"); dir != "" {
		return filepath.Join(dir, *runnerBinPath)
	}
	if dir := os.Getenv("TEST_SRCDIR"); dir != "" {
		return filepath.Join(dir, *runnerBinPath)
	}
	return *runnerBinPath
}

// invocation records a single call to the fake tofu binary.
type invocation struct {
	cwd  string
	args []string
}

func (inv invocation) hasArg(arg string) bool {
	for _, a := range inv.args {
		if a == arg {
			return true
		}
	}
	return false
}

func (inv invocation) hasArgWithPrefix(prefix string) bool {
	for _, a := range inv.args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

// parseInvocations reads the structured log written by the fake tofu script.
// Each invocation is a block of lines:
//
//	cwd=<working directory>
//	arg=<arg1>
//	arg=<arg2>
//	---
func parseInvocations(data []byte) []invocation {
	var invs []invocation
	var cur *invocation
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case line == "---":
			if cur != nil {
				invs = append(invs, *cur)
				cur = nil
			}
		case strings.HasPrefix(line, "cwd="):
			if cur == nil {
				cur = &invocation{}
			}
			cur.cwd = strings.TrimPrefix(line, "cwd=")
		case strings.HasPrefix(line, "arg="):
			if cur == nil {
				cur = &invocation{}
			}
			cur.args = append(cur.args, strings.TrimPrefix(line, "arg="))
		}
	}
	return invs
}

// setup creates a temporary work tree with the given vars written to
// rules_tofu.auto.tfvars.json, and a fake tofu script that logs each invocation
// to a temp file. It returns:
//   - a pre-configured runner Cmd (callers may append --var-file flags before running)
//   - addVarFile: writes a .tfvars.json file and returns its path
//   - invocations: reads and returns all recorded tofu invocations
func setup(t *testing.T, vars map[string]any) (
	cmd *exec.Cmd,
	addVarFile func(map[string]any) string,
	invocations func() []invocation,
) {
	t.Helper()
	return setupPkg(t, vars, "mypkg")
}

// setupPkg is like setup but places the deploy at the given workspace-relative
// package directory. An empty pkgDir models a deploy in the workspace root
// package, where the work tree root itself is the cwd.
func setupPkg(t *testing.T, vars map[string]any, pkgDir string) (
	cmd *exec.Cmd,
	addVarFile func(map[string]any) string,
	invocations func() []invocation,
) {
	t.Helper()
	bin := runnerBin(t)
	dir := t.TempDir()

	logPath := filepath.Join(dir, "tofu-invocations.log")
	fakeTofuSh := filepath.Join(dir, "tofu.sh")
	script := "#!/bin/sh\n" +
		"{ printf 'cwd=%s\\n' \"$(pwd)\"; for a in \"$@\"; do printf 'arg=%s\\n' \"$a\"; done; printf -- '---\\n'; } >> \"$TOFU_LOG\"\n" +
		"exit 0\n"
	if err := os.WriteFile(fakeTofuSh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	workTree := filepath.Join(dir, "work")
	if err := os.MkdirAll(filepath.Join(workTree, pkgDir), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(workTree, pkgDir, "rules_tofu.auto.tfvars.json"), vars)

	var varFileCounter int
	addVarFile = func(contents map[string]any) string {
		varFileCounter++
		path := filepath.Join(dir, fmt.Sprintf("varfile%d.tfvars.json", varFileCounter))
		writeJSON(t, path, contents)
		return path
	}

	invocations = func() []invocation {
		t.Helper()
		data, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("read tofu invocation log: %v", err)
		}
		return parseInvocations(data)
	}

	pluginDir := filepath.Join(workTree, pkgDir, ".rules_tofu-plugins")

	cmd = exec.Command(bin,
		"--tofu="+fakeTofuSh,
		"--work-tree="+workTree,
		"--package-dir="+pkgDir,
		"--plugin-dir="+pluginDir,
		"--state-dir="+filepath.Join(dir, "state"),
		"--command=plan",
	)
	cmd.Env = append(os.Environ(),
		"BUILD_WORKSPACE_DIRECTORY="+dir,
		"TOFU_LOG="+logPath,
	)
	return cmd, addVarFile, invocations
}

func TestRunner_NoVarFiles(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	// Expect: init, plan.
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	init := invs[0]
	if !init.hasArg("init") {
		t.Errorf("first invocation should be 'init', got args: %v", init.args)
	}
	if !init.hasArg("-input=false") {
		t.Errorf("init invocation missing -input=false, got args: %v", init.args)
	}
	if !init.hasArgWithPrefix("-plugin-dir=") {
		t.Errorf("init invocation missing -plugin-dir=, got args: %v", init.args)
	}
	plan := invs[1]
	if !plan.hasArg("plan") {
		t.Errorf("second invocation should be 'plan', got args: %v", plan.args)
	}
	if !plan.hasArg("-input=false") {
		t.Errorf("plan invocation missing -input=false, got args: %v", plan.args)
	}
	if plan.hasArgWithPrefix("-var-file=") {
		t.Errorf("plan invocation should have no -var-file, got args: %v", plan.args)
	}
}

func TestRunner_VarFileNoOverlap(t *testing.T) {
	cmd, addVarFile, invocations := setup(t, map[string]any{"region": "us-east-1"})
	vf := addVarFile(map[string]any{"env": "prod"})
	cmd.Args = append(cmd.Args, "--var-file="+vf)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	plan := invs[1]
	if !plan.hasArg("-var-file=" + vf) {
		t.Errorf("plan invocation missing -var-file=%s, got args: %v", vf, plan.args)
	}
	if !plan.hasArg("-input=false") {
		t.Errorf("plan invocation missing -input=false, got args: %v", plan.args)
	}
}

// TestRunner_WithDataFile verifies that a work tree containing arbitrary data
// files (e.g. placed there by the `data` attribute) does not interfere with
// normal runner execution. The files are simply present on disk so that
// Terraform's file() function can read them.
func TestRunner_WithDataFile(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})

	// Simulate a data file materialized by the `data` attribute — its location
	// is determined at analysis time by the Bazel rule (short_path under the
	// work tree), so here we just place it inside the work tree directory.
	workTree := ""
	for _, arg := range cmd.Args {
		if strings.HasPrefix(arg, "--work-tree=") {
			workTree = strings.TrimPrefix(arg, "--work-tree=")
		}
	}
	if workTree == "" {
		t.Fatal("could not find --work-tree flag in runner cmd args")
	}
	dataPath := filepath.Join(workTree, "mypkg", "config.json")
	if err := os.WriteFile(dataPath, []byte(`{"key":"value"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed with data file present: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	if !invs[0].hasArg("init") {
		t.Errorf("first invocation should be 'init', got args: %v", invs[0].args)
	}
	if !invs[1].hasArg("plan") {
		t.Errorf("second invocation should be 'plan', got args: %v", invs[1].args)
	}
}

func TestRunner_ExtraArgsPassedToPlan(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	cmd.Args = append(cmd.Args, "--", "--target=aws_instance.foo")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	plan := invs[1]
	if !plan.hasArg("--target=aws_instance.foo") {
		t.Errorf("plan invocation missing extra arg, got args: %v", plan.args)
	}
}

func TestRunner_ExtraArgsPassedToApply(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	cmd.Args = append(cmd.Args,
		"--command=apply",
		// No backend block here, so apply needs the ephemeral-state opt-in.
		"--allow-ephemeral-state",
		"--", "--replace=aws_instance.foo",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	// Expect: init, apply.
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, apply), got %d: %+v", len(invs), invs)
	}
	apply := invs[1]
	if !apply.hasArg("apply") {
		t.Errorf("second invocation should be 'apply', got args: %v", apply.args)
	}
	if !apply.hasArg("--replace=aws_instance.foo") {
		t.Errorf("apply invocation missing extra arg, got args: %v", apply.args)
	}
}

func TestRunner_ExtraArgsPassedToDestroy(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	cmd.Args = append(cmd.Args,
		"--command=destroy",
		// No backend block here, so destroy needs the ephemeral-state opt-in.
		"--allow-ephemeral-state",
		"--", "--target=aws_instance.foo",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	// Expect: init, destroy.
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, destroy), got %d: %+v", len(invs), invs)
	}
	destroy := invs[1]
	if !destroy.hasArg("destroy") {
		t.Errorf("second invocation should be 'destroy', got args: %v", destroy.args)
	}
	if !destroy.hasArg("--target=aws_instance.foo") {
		t.Errorf("destroy invocation missing extra arg, got args: %v", destroy.args)
	}
}

// TestRunner_StateFlags_Apply verifies that the apply invocation receives
// -state= and -state-out= flags when no backend block is present. The
// ephemeral-state opt-in is required to reach apply at all in that shape.
func TestRunner_StateFlags_Apply(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	cmd.Args = append(cmd.Args, "--command=apply", "--allow-ephemeral-state")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	// Expect: init, apply.
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, apply), got %d: %+v", len(invs), invs)
	}
	apply := invs[1]
	if !apply.hasArg("apply") {
		t.Errorf("second invocation should be 'apply', got args: %v", apply.args)
	}
	if !apply.hasArgWithPrefix("-state=") {
		t.Errorf("apply invocation missing -state=, got args: %v", apply.args)
	}
	if !apply.hasArgWithPrefix("-state-out=") {
		t.Errorf("apply invocation missing -state-out=, got args: %v", apply.args)
	}
}

// TestRunner_StateFlags_Destroy verifies that the destroy invocation receives
// -state= and -state-out= flags when no backend block is present. The
// ephemeral-state opt-in is required to reach destroy at all in that shape.
func TestRunner_StateFlags_Destroy(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	cmd.Args = append(cmd.Args, "--command=destroy", "--allow-ephemeral-state")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	// Expect: init, destroy.
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, destroy), got %d: %+v", len(invs), invs)
	}
	destroy := invs[1]
	if !destroy.hasArg("destroy") {
		t.Errorf("second invocation should be 'destroy', got args: %v", destroy.args)
	}
	if !destroy.hasArgWithPrefix("-state=") {
		t.Errorf("destroy invocation missing -state=, got args: %v", destroy.args)
	}
	if !destroy.hasArgWithPrefix("-state-out=") {
		t.Errorf("destroy invocation missing -state-out=, got args: %v", destroy.args)
	}
}

// backendTFContent is a helper that writes a .tf file with a terraform block
// containing the given inner content into the package directory of the work
// tree, and returns the path written.
func backendTFContent(t *testing.T, cmd *exec.Cmd, content string) {
	t.Helper()
	var workTree string
	for _, arg := range cmd.Args {
		if strings.HasPrefix(arg, "--work-tree=") {
			workTree = strings.TrimPrefix(arg, "--work-tree=")
		}
	}
	if workTree == "" {
		t.Fatal("could not find --work-tree flag in runner cmd args")
	}
	tfPath := filepath.Join(workTree, "mypkg", "backend_override.tf")
	if err := os.WriteFile(tfPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRunner_BackendBlock_OmitsStateFlags verifies that when a terraform{}
// backend "..." {} block is present, the runner does not pass -state= or
// -state-out= to tofu.
func TestRunner_BackendBlock_OmitsStateFlags(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	backendTFContent(t, cmd, `
terraform {
  backend "local" {}
}
`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	plan := invs[1]
	if plan.hasArgWithPrefix("-state=") {
		t.Errorf("plan invocation should not have -state= when backend block present, got args: %v", plan.args)
	}
	if plan.hasArgWithPrefix("-state-out=") {
		t.Errorf("plan invocation should not have -state-out= when backend block present, got args: %v", plan.args)
	}
}

// TestRunner_CloudBlock_OmitsStateFlags verifies that when a terraform{}
// cloud {} block is present (the modern HCP Terraform alternative to backend),
// the runner does not pass -state= or -state-out= to tofu.
func TestRunner_CloudBlock_OmitsStateFlags(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	backendTFContent(t, cmd, `
terraform {
  cloud {}
}
`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	plan := invs[1]
	if plan.hasArgWithPrefix("-state=") {
		t.Errorf("plan invocation should not have -state= when cloud block present, got args: %v", plan.args)
	}
	if plan.hasArgWithPrefix("-state-out=") {
		t.Errorf("plan invocation should not have -state-out= when cloud block present, got args: %v", plan.args)
	}
}

// writePkgFile writes a file with the given basename and content into the
// package directory of the work tree.
func writePkgFile(t *testing.T, cmd *exec.Cmd, name, content string) {
	t.Helper()
	var workTree string
	for _, arg := range cmd.Args {
		if strings.HasPrefix(arg, "--work-tree=") {
			workTree = strings.TrimPrefix(arg, "--work-tree=")
		}
	}
	if workTree == "" {
		t.Fatal("could not find --work-tree flag in runner cmd args")
	}
	path := filepath.Join(workTree, "mypkg", name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRunner_BackendBlock_JSON_OmitsStateFlags verifies that a backend block
// declared in Terraform JSON syntax (.tf.json) is recognised, so the runner
// omits -state= / -state-out= just as it does for native .tf syntax.
func TestRunner_BackendBlock_JSON_OmitsStateFlags(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	writePkgFile(t, cmd, "backend.tf.json", `{"terraform": {"backend": {"s3": {}}}}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	plan := invs[1]
	if plan.hasArgWithPrefix("-state=") {
		t.Errorf("plan invocation should not have -state= when .tf.json backend block present, got args: %v", plan.args)
	}
	if plan.hasArgWithPrefix("-state-out=") {
		t.Errorf("plan invocation should not have -state-out= when .tf.json backend block present, got args: %v", plan.args)
	}
}

// TestRunner_NoBackendBlock_JSON_KeepsStateFlags verifies that a .tf.json file
// with a terraform block but no backend/cloud block does not suppress the local
// -state= / -state-out= flags.
func TestRunner_NoBackendBlock_JSON_KeepsStateFlags(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	writePkgFile(t, cmd, "main.tf.json", `{"terraform": {"required_version": ">= 1.0"}}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	plan := invs[1]
	if !plan.hasArgWithPrefix("-state=") {
		t.Errorf("plan invocation should have -state= when .tf.json has no backend block, got args: %v", plan.args)
	}
	if !plan.hasArgWithPrefix("-state-out=") {
		t.Errorf("plan invocation should have -state-out= when .tf.json has no backend block, got args: %v", plan.args)
	}
}

// TestRunner_MalformedJSON_KeepsStateFlags verifies that an unparseable .tf.json
// file is treated as backend-less: the runner keeps the local -state= /
// -state-out= flags and does not crash. Build-time validate guarantees
// well-formed files at runtime, so this is a defensive backstop only.
func TestRunner_MalformedJSON_KeepsStateFlags(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	writePkgFile(t, cmd, "broken.tf.json", `{"terraform": {`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly with malformed .tf.json present: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	plan := invs[1]
	if !plan.hasArgWithPrefix("-state=") {
		t.Errorf("plan invocation should have -state= when .tf.json is malformed, got args: %v", plan.args)
	}
	if !plan.hasArgWithPrefix("-state-out=") {
		t.Errorf("plan invocation should have -state-out= when .tf.json is malformed, got args: %v", plan.args)
	}
}

// TestRunner_MissingBuildWorkspaceDirectory verifies that the runner exits
// non-zero and prints an actionable message when BUILD_WORKSPACE_DIRECTORY
// is not set (i.e. it was invoked outside of `bazel run`).
func TestRunner_MissingBuildWorkspaceDirectory(t *testing.T) {
	cmd, _, _ := setup(t, map[string]any{"region": "us-east-1"})
	// Strip BUILD_WORKSPACE_DIRECTORY from the environment.
	filtered := cmd.Env[:0]
	for _, e := range cmd.Env {
		if !strings.HasPrefix(e, "BUILD_WORKSPACE_DIRECTORY=") {
			filtered = append(filtered, e)
		}
	}
	cmd.Env = filtered
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected runner to fail when BUILD_WORKSPACE_DIRECTORY is unset, but it succeeded")
	}
	msg := string(out)
	if !strings.Contains(msg, "BUILD_WORKSPACE_DIRECTORY") && !strings.Contains(msg, "bazel run") {
		t.Errorf("expected output to mention BUILD_WORKSPACE_DIRECTORY or bazel run, got: %s", msg)
	}
}

// TestRunner_MissingRequiredFlags verifies that omitting any single required
// flag causes the runner to exit non-zero with a message naming the flag.
func TestRunner_MissingRequiredFlags(t *testing.T) {
	requiredFlags := []string{
		"--tofu",
		"--work-tree",
		"--command",
		"--state-dir",
		"--plugin-dir",
	}
	for _, flagName := range requiredFlags {
		flagName := flagName
		t.Run(flagName, func(t *testing.T) {
			cmd, _, _ := setup(t, map[string]any{"region": "us-east-1"})
			// Remove the flag from Args, keeping all others.
			filtered := cmd.Args[:1] // keep argv[0] (binary path)
			for _, arg := range cmd.Args[1:] {
				if strings.HasPrefix(arg, flagName+"=") || arg == flagName {
					continue
				}
				filtered = append(filtered, arg)
			}
			cmd.Args = filtered
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("expected runner to fail when %s is omitted, but it succeeded", flagName)
			}
			if !strings.Contains(string(out), flagName) {
				t.Errorf("expected output to mention %s, got: %s", flagName, out)
			}
		})
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRunner_BackendInSiblingDir verifies that a backend block located in a
// sibling directory of the package dir (simulating a transitive tf_library dep
// materialised at its own workspace-relative path) does NOT suppress the
// -state= / -state-out= flags.
//
// Terraform reads backend configuration only from the root module — the
// directory where `tofu init` is invoked (packageDir). Files from library deps
// land at their own workspace-relative paths and are child modules, not the
// root module, so their backend blocks are irrelevant to the root module's
// state backend. hasBackend intentionally scans only the package directory.
func TestRunner_BackendInSiblingDir(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})

	// Find the work tree root from the runner args.
	var workTree string
	for _, arg := range cmd.Args {
		if strings.HasPrefix(arg, "--work-tree=") {
			workTree = strings.TrimPrefix(arg, "--work-tree=")
		}
	}
	if workTree == "" {
		t.Fatal("could not find --work-tree flag in runner cmd args")
	}

	// Simulate a tf_library dep materialised at a sibling path (e.g.
	// "somelib/" next to "mypkg/"). Place a backend block there — it should
	// be invisible to hasBackend, which only scans the package dir.
	siblingDir := filepath.Join(workTree, "somelib")
	if err := os.MkdirAll(siblingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	siblingBackend := filepath.Join(siblingDir, "backend.tf")
	if err := os.WriteFile(siblingBackend, []byte(`
terraform {
  backend "s3" {}
}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	plan := invs[1]
	// Backend block is in a sibling dir, not in the package dir — the runner
	// must still pass -state= and -state-out= for the root module.
	if !plan.hasArgWithPrefix("-state=") {
		t.Errorf("plan invocation should have -state= when backend is only in sibling dir, got args: %v", plan.args)
	}
	if !plan.hasArgWithPrefix("-state-out=") {
		t.Errorf("plan invocation should have -state-out= when backend is only in sibling dir, got args: %v", plan.args)
	}
}

// argValue returns the value of the named --flag=value entry in the runner's
// args, failing the test if the flag is absent.
func argValue(t *testing.T, cmd *exec.Cmd, flagName string) string {
	t.Helper()
	prefix := flagName + "="
	for _, arg := range cmd.Args {
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimPrefix(arg, prefix)
		}
	}
	t.Fatalf("could not find %s flag in runner cmd args", flagName)
	return ""
}

// envValue returns the value of the named variable in the runner's environment,
// failing the test if it is absent.
func envValue(t *testing.T, cmd *exec.Cmd, name string) string {
	t.Helper()
	prefix := name + "="
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix)
		}
	}
	t.Fatalf("could not find %s in runner cmd env", name)
	return ""
}

// assertRefusedEphemeralState asserts that the runner refused a mutating
// command: it exited non-zero, named the opt-in attribute, ran no tofu process
// at all, and did not create the state directory.
func assertRefusedEphemeralState(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	logPath := envValue(t, cmd, "TOFU_LOG")
	stateDir := argValue(t, cmd, "--state-dir")

	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected runner to refuse local state, but it succeeded:\n%s", out)
	}
	msg := string(out)
	if !strings.Contains(msg, "allow_ephemeral_state") {
		t.Errorf("expected refusal to name allow_ephemeral_state, got: %s", msg)
	}
	if !strings.Contains(msg, stateDir) {
		t.Errorf("expected refusal to name the state path %s, got: %s", stateDir, msg)
	}

	// The gate must fail before any tofu process starts, so the fake tofu's
	// log must be absent (or, defensively, empty). invocations() cannot be used
	// here: it t.Fatal's on a missing log file.
	if info, err := os.Stat(logPath); err == nil {
		if info.Size() > 0 {
			data, readErr := os.ReadFile(logPath)
			if readErr != nil {
				t.Fatalf("read tofu invocation log: %v", readErr)
			}
			t.Errorf("expected no tofu invocations, got: %+v", parseInvocations(data))
		}
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat tofu invocation log: %v", err)
	}

	if _, err := os.Stat(stateDir); err == nil {
		t.Errorf("state dir %s should not have been created", stateDir)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat state dir: %v", err)
	}
}

// TestRunner_EphemeralState_ApplyRefused verifies that apply against a deploy
// with no backend block and no opt-in is refused outright: no tofu runs and the
// state directory is not created.
func TestRunner_EphemeralState_ApplyRefused(t *testing.T) {
	cmd, _, _ := setup(t, map[string]any{"region": "us-east-1"})
	cmd.Args = append(cmd.Args, "--command=apply")
	assertRefusedEphemeralState(t, cmd)
}

// TestRunner_EphemeralState_DestroyRefused verifies that destroy is gated the
// same way as apply.
func TestRunner_EphemeralState_DestroyRefused(t *testing.T) {
	cmd, _, _ := setup(t, map[string]any{"region": "us-east-1"})
	cmd.Args = append(cmd.Args, "--command=destroy")
	assertRefusedEphemeralState(t, cmd)
}

// TestRunner_EphemeralState_PlanUngated verifies that plan is deliberately not
// gated: planning against empty local state destroys nothing, so it runs and
// still receives the local -state= / -state-out= flags.
func TestRunner_EphemeralState_PlanUngated(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("plan should not be gated, but the runner failed: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	plan := invs[1]
	if !plan.hasArg("plan") {
		t.Errorf("second invocation should be 'plan', got args: %v", plan.args)
	}
	if !plan.hasArgWithPrefix("-state=") {
		t.Errorf("plan invocation missing -state=, got args: %v", plan.args)
	}
	if !plan.hasArgWithPrefix("-state-out=") {
		t.Errorf("plan invocation missing -state-out=, got args: %v", plan.args)
	}
}

// TestRunner_EphemeralState_ApplyAllowed verifies that --allow-ephemeral-state
// restores today's behaviour: apply runs and keeps the local state flags.
func TestRunner_EphemeralState_ApplyAllowed(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	cmd.Args = append(cmd.Args, "--command=apply", "--allow-ephemeral-state")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly with --allow-ephemeral-state: %v\n%s", err, out)
	}
	invs := invocations()
	// Expect: init, apply.
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, apply), got %d: %+v", len(invs), invs)
	}
	if !invs[0].hasArg("init") {
		t.Errorf("first invocation should be 'init', got args: %v", invs[0].args)
	}
	apply := invs[1]
	if !apply.hasArg("apply") {
		t.Errorf("second invocation should be 'apply', got args: %v", apply.args)
	}
	if !apply.hasArgWithPrefix("-state=") {
		t.Errorf("apply invocation missing -state=, got args: %v", apply.args)
	}
	if !apply.hasArgWithPrefix("-state-out=") {
		t.Errorf("apply invocation missing -state-out=, got args: %v", apply.args)
	}
}

// TestRunner_EphemeralState_DestroyAllowed verifies the opt-in applies to
// destroy as well as apply.
func TestRunner_EphemeralState_DestroyAllowed(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	cmd.Args = append(cmd.Args, "--command=destroy", "--allow-ephemeral-state")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly with --allow-ephemeral-state: %v\n%s", err, out)
	}
	invs := invocations()
	// Expect: init, destroy.
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, destroy), got %d: %+v", len(invs), invs)
	}
	destroy := invs[1]
	if !destroy.hasArg("destroy") {
		t.Errorf("second invocation should be 'destroy', got args: %v", destroy.args)
	}
	if !destroy.hasArgWithPrefix("-state=") {
		t.Errorf("destroy invocation missing -state=, got args: %v", destroy.args)
	}
	if !destroy.hasArgWithPrefix("-state-out=") {
		t.Errorf("destroy invocation missing -state-out=, got args: %v", destroy.args)
	}
}

// TestRunner_EphemeralState_BackendApplyAllowed verifies that the gate keys off
// backend detection rather than the opt-in flag: with a backend block present,
// apply runs without the flag and the local state flags stay omitted.
func TestRunner_EphemeralState_BackendApplyAllowed(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	backendTFContent(t, cmd, `
terraform {
  backend "s3" {}
}
`)
	cmd.Args = append(cmd.Args, "--command=apply")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly with a backend block present: %v\n%s", err, out)
	}
	invs := invocations()
	// Expect: init, apply.
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, apply), got %d: %+v", len(invs), invs)
	}
	apply := invs[1]
	if !apply.hasArg("apply") {
		t.Errorf("second invocation should be 'apply', got args: %v", apply.args)
	}
	if apply.hasArgWithPrefix("-state=") {
		t.Errorf("apply invocation should not have -state= when backend block present, got args: %v", apply.args)
	}
	if apply.hasArgWithPrefix("-state-out=") {
		t.Errorf("apply invocation should not have -state-out= when backend block present, got args: %v", apply.args)
	}
}

// TestRunner_EmptyPackageDir_RunsAtWorkTreeRoot verifies that an empty
// --package-dir is valid: it models a deploy in the workspace root package, so
// tofu runs with the work tree root itself as its cwd. (The BCR test module
// e2e/smoke has exactly this shape.)
func TestRunner_EmptyPackageDir_RunsAtWorkTreeRoot(t *testing.T) {
	cmd, _, invocations := setupPkg(t, map[string]any{"region": "us-east-1"}, "")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly with empty --package-dir: %v\n%s", err, out)
	}
	invs := invocations()
	// Expect: init, plan.
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	if !invs[0].hasArg("init") {
		t.Errorf("first invocation should be 'init', got args: %v", invs[0].args)
	}
	if !invs[1].hasArg("plan") {
		t.Errorf("second invocation should be 'plan', got args: %v", invs[1].args)
	}

	var workTree string
	for _, arg := range cmd.Args {
		if strings.HasPrefix(arg, "--work-tree=") {
			workTree = strings.TrimPrefix(arg, "--work-tree=")
		}
	}
	if workTree == "" {
		t.Fatal("could not find --work-tree flag in runner cmd args")
	}
	// The recorded cwd must be the work tree root itself, not a package
	// subdirectory. Resolve symlinks on both sides: the fake tofu records the
	// physical path, while --work-tree carries the logical path.
	wantCwd, err := filepath.EvalSymlinks(workTree)
	if err != nil {
		t.Fatalf("resolve work tree root: %v", err)
	}
	for _, inv := range invs {
		gotCwd, err := filepath.EvalSymlinks(inv.cwd)
		if err != nil {
			t.Fatalf("resolve recorded cwd %q: %v", inv.cwd, err)
		}
		if gotCwd != wantCwd {
			t.Errorf("tofu cwd = %q, want work tree root %q", gotCwd, wantCwd)
		}
	}
}
