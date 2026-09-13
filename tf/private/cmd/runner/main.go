// Command runner is rules_tofu's tofu plan/apply wrapper. It is invoked by
// the generated shell launcher emitted by the tf_runner rule with every
// configuration value passed via explicit standard flags.
//
// Responsibilities:
//   - Validate we were invoked via `bazel run` (BUILD_WORKSPACE_DIRECTORY set).
//   - Refuse apply/destroy when the deploy declares no state backend and has
//     not opted in via --allow-ephemeral-state, since local state under
//     bazel-bin does not survive `bazel clean`.
//   - Run `tofu init` inside the pre-materialized work tree.
//   - For plan: emit a plan artifact and stop.
//   - For apply/destroy: delegate to tofu, passing through any extra args.
//
// Configuration validation (`tofu init -backend=false && tofu validate`)
// happens at `bazel build` time via the deploy rule's TofuValidate action,
// not here. Duplicate-variable-key detection across `vars` and `var_files`
// likewise happens at build time via the dupcheck action.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
)

// stringList is a repeatable string flag (e.g. --var-file can appear multiple times).
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func stringListFlag(name, usage string) *stringList {
	sl := new(stringList)
	flag.Var(sl, name, usage)
	return sl
}

var (
	workTree   = flag.String("work-tree", "", "path to the materialized work tree root")
	packageDir = flag.String("package-dir", "", "workspace-relative dir to cd into within the work tree")
	pluginDir  = flag.String("plugin-dir", "", "absolute path to the vendored provider plugin tree, passed to tofu init via -plugin-dir")
	varFiles   = stringListFlag("var-file", "path to a .tfvars.json file passed to tofu via -var-file (repeatable)")

	stateDir = flag.String("state-dir", "", "absolute path to the per-deploy state directory")

	allowEphemeralState = flag.Bool("allow-ephemeral-state", false,
		"permit apply/destroy against local state under bazel-bin when the deploy declares no backend; "+
			"set by tf_deploy(allow_ephemeral_state = True)")

	tofu    = flag.String("tofu", "", "path to the tofu binary")
	command = flag.String("command", "", `"plan", "apply", or "destroy"`)
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("rules_tofu: ")
	flag.Parse()

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		os.Exit(1)
	}
}

func run() error {
	extraArgs := flag.Args()

	// --package-dir is deliberately absent: an empty value is valid and means
	// the deploy lives in the workspace root package, so the work tree root
	// itself is the cwd.
	for name, value := range map[string]string{
		"--tofu":       *tofu,
		"--work-tree":  *workTree,
		"--command":    *command,
		"--state-dir":  *stateDir,
		"--plugin-dir": *pluginDir,
	} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if *command != "plan" && *command != "apply" && *command != "destroy" {
		return fmt.Errorf(`--command must be "plan", "apply", or "destroy", got %q`, *command)
	}

	if os.Getenv("BUILD_WORKSPACE_DIRECTORY") == "" {
		return errors.New(
			"rules_tofu runner must be invoked via `bazel run`. " +
				"BUILD_WORKSPACE_DIRECTORY is unset, so state cannot be persisted.",
		)
	}
	cwd := filepath.Join(*workTree, *packageDir)
	if _, err := os.Stat(cwd); err != nil {
		return fmt.Errorf("work-tree cwd %s: %w", cwd, err)
	}

	// Backend detection is a read-only scan of the root module's .tf files, so
	// it does not depend on init having run. Compute it once, up front: the
	// ephemeral-state gate below needs the answer before any tofu process is
	// started or any directory is created, and the state flags reuse it.
	backend := hasBackend(cwd)
	planFile := filepath.Join(*stateDir, "tfplan")
	stateFile := filepath.Join(*stateDir, "terraform.tfstate")

	// Refuse to mutate real infrastructure against throwaway local state.
	// Deliberately fails ahead of everything else: no tofu runs and the state
	// directory is not created. `plan` is ungated — planning against empty
	// local state destroys nothing.
	if (*command == "apply" || *command == "destroy") && !backend && !*allowEphemeralState {
		return ephemeralStateError(*command, stateFile)
	}

	if err := os.MkdirAll(*stateDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", *stateDir, err)
	}

	// TF_IN_AUTOMATION=1 suppresses usage-hint lines in tofu output that
	// don't apply in our wrapped invocation (e.g. "Run `terraform plan`…").
	// We deliberately do NOT set TF_INPUT=0 globally: variable prompts are
	// suppressed per-command via -input=false, but the apply/destroy
	// approval prompt must remain reachable.
	env := append(os.Environ(), "TF_IN_AUTOMATION=1")

	// Ensure the vendored plugin tree exists even when zero providers are in
	// scope (the deploy declares no symlinks under the sibling <name>.plugins/
	// tree in that case, so the runfiles tree lacks the directory).
	// -plugin-dir overrides all default plugin search paths and prevents the
	// registry from being contacted at runtime.
	if err := os.MkdirAll(*pluginDir, 0o755); err != nil {
		return fmt.Errorf("create plugin dir %s: %w", *pluginDir, err)
	}
	if err := runTofu(env, cwd, "init", "-input=false", "-plugin-dir="+*pluginDir); err != nil {
		return fmt.Errorf("tofu init: %w", err)
	}
	stateArgs := []string{"-state=" + stateFile, "-state-out=" + stateFile}
	if backend {
		stateArgs = nil
	}
	varFileArgs := make([]string, len(*varFiles))
	for i, f := range *varFiles {
		varFileArgs[i] = "-var-file=" + f
	}

	switch *command {
	case "plan":
		args := append([]string{"plan", "-input=false", "-out=" + planFile}, varFileArgs...)
		args = append(args, stateArgs...)
		args = append(args, extraArgs...)
		if err := runTofu(env, cwd, args...); err != nil {
			return fmt.Errorf("tofu plan: %w", err)
		}
		fmt.Printf("rules_tofu: plan saved to %s\n", planFile)
	case "apply":
		applyArgs := append([]string{"apply", "-input=false"}, varFileArgs...)
		applyArgs = append(applyArgs, stateArgs...)
		applyArgs = append(applyArgs, extraArgs...)
		if err := runTofu(env, cwd, applyArgs...); err != nil {
			return fmt.Errorf("tofu apply: %w", err)
		}
	case "destroy":
		destroyArgs := append([]string{"destroy", "-input=false"}, varFileArgs...)
		destroyArgs = append(destroyArgs, stateArgs...)
		destroyArgs = append(destroyArgs, extraArgs...)
		if err := runTofu(env, cwd, destroyArgs...); err != nil {
			return fmt.Errorf("tofu destroy: %w", err)
		}
	}
	return nil
}

// ephemeralStateError is the refusal returned when apply/destroy would write
// local state. It is the whole user-facing surface of the gate, so it names the
// state path, the recommended fix, and the opt-in verbatim.
func ephemeralStateError(command, stateFile string) error {
	return fmt.Errorf(`rules_tofu: refusing to run "tofu %s": this deploy declares no state backend, so its state would be written to

    %s

That path is machine-local and disposable. It sits under bazel-bin, so
"bazel clean" deletes it; it embeds the build configuration, so building with
-c opt or a different --platforms silently forks to a different state file; and
a colleague running this target from their own checkout gets their own private
copy. Losing or forking that file orphans every resource this command creates:
the infrastructure carries on existing with nothing tracking it.

Recommended fix — declare a remote backend in a .tf file listed in this
deploy's srcs:

    terraform {
      backend "gcs" {
        bucket = "my-tofu-state"
        prefix = "prod"
      }
    }

(Any backend, or a cloud {} block, will do.) With one present, the runner
defers to it entirely and this check no longer applies.

If disposable state is the point — a demo or example, a throwaway sandbox, or
bootstrapping the bucket that will later hold real state — opt in explicitly by
setting

    allow_ephemeral_state = True

on the tf_deploy target. For the bootstrap case, add the backend block once the
bucket exists and move the existing state into it with:

    tofu init -migrate-state

The deploy's .plan target is never gated, so planning still works either way.`,
		command, stateFile)
}

func runTofu(env []string, cwd string, args ...string) error {
	cmd := exec.Command(*tofu, args...)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// terraformBlockSchema matches top-level `terraform {}` blocks; backendBlockSchema
// matches the `backend "type" {}` and `cloud {}` blocks nested within one. Both
// are used with schema-based PartialContent, which decodes native (.tf) and JSON
// (.tf.json) syntax uniformly and ignores every other construct.
var (
	terraformBlockSchema = &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{{Type: "terraform"}},
	}
	backendBlockSchema = &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "backend", LabelNames: []string{"type"}},
			{Type: "cloud"},
		},
	}
)

// hasBackend reports whether any .tf or .tf.json file in the deploy's package
// directory declares a `backend "..." {}` or `cloud {}` block nested inside a
// top-level `terraform {}` block. Either construct means the workspace uses a
// remote/cloud state backend, and local -state/-state-out flags should be
// omitted from the runner invocation.
//
// A file that fails to parse is treated as backend-less. The build-time validate
// action already guarantees well-formed files at runtime, so a parse error here
// must not crash the runner.
//
// Only the package directory (cwd) is scanned — not the full work tree. This is
// intentional: Terraform reads backend configuration from the root module only,
// which is the directory where `tofu init` runs. Transitive library deps are
// materialised at their own workspace-relative paths (e.g. sibling directories)
// and are not the root module, so backend blocks in those files do not affect
// the root module's state backend.
func hasBackend(cwd string) bool {
	entries, err := os.ReadDir(cwd)
	if err != nil {
		return false
	}
	parser := hclparse.NewParser()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		isJSON := strings.HasSuffix(name, ".tf.json")
		if !isJSON && filepath.Ext(name) != ".tf" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(cwd, name))
		if err != nil {
			continue
		}
		var file *hcl.File
		var diags hcl.Diagnostics
		if isJSON {
			file, diags = parser.ParseJSON(b, name)
		} else {
			file, diags = parser.ParseHCL(b, name)
		}
		if diags.HasErrors() {
			continue
		}
		if containsBackendBlock(file) {
			return true
		}
	}
	return false
}

func containsBackendBlock(file *hcl.File) bool {
	content, _, diags := file.Body.PartialContent(terraformBlockSchema)
	if diags.HasErrors() {
		return false
	}
	for _, block := range content.Blocks {
		inner, _, diags := block.Body.PartialContent(backendBlockSchema)
		if diags.HasErrors() {
			continue
		}
		if len(inner.Blocks) > 0 {
			return true
		}
	}
	return false
}
