# Notes for Claude Code

Public docs are in `README.md`. This file is the agent-specific overlay:
sandbox bootstrap, hard invariants, repo conventions. Don't add general
documentation here.

Respond in British English.

## Build / test in the Anthropic sandbox

`bcr.bazel.build` is not in the proxy allowlist. Before the first build,
write `.bazelrc.local` (gitignored; the committed `.bazelrc` already
`try-import`s it) using the llmbox bootstrap script. Run once per session:

```python
import os

proxy = os.environ.get("GLOBAL_AGENT_HTTP_PROXY", "")

lines = [
    "# Auto-generated — do not commit (gitignored)",
    "",
    "common --registry=https://raw.githubusercontent.com/bazelbuild/bazel-central-registry/main",
    "",
    "startup --host_jvm_args=-Djavax.net.ssl.trustStore=/etc/ssl/certs/java/cacerts",
    "startup --host_jvm_args=-Djavax.net.ssl.trustStorePassword=changeit",
]

if proxy:
    no_scheme = proxy[len("http://"):]
    userinfo, hostport = no_scheme.rsplit("@", 1)
    host, port = hostport.rsplit(":", 1)
    user, password = userinfo.split(":", 1)
    lines += [
        "",
        f"startup --host_jvm_args=-Dhttps.proxyHost={host}",
        f"startup --host_jvm_args=-Dhttps.proxyPort={port}",
        f"startup --host_jvm_args=-Dhttps.proxyUser={user}",
        f"startup --host_jvm_args=-Dhttps.proxyPassword={password}",
        f"startup --host_jvm_args=-Dhttp.proxyHost={host}",
        f"startup --host_jvm_args=-Dhttp.proxyPort={port}",
        f"startup --host_jvm_args=-Dhttp.proxyUser={user}",
        f"startup --host_jvm_args=-Dhttp.proxyPassword={password}",
        "startup --host_jvm_args=-Dhttp.nonProxyHosts=localhost|127.*|[::1]",
        "",
        "startup --host_jvm_args=-Djdk.http.auth.tunneling.disabledSchemes=",
        "startup --host_jvm_args=-Djdk.http.auth.proxying.disabledSchemes=",
    ]

with open(".bazelrc.local", "w") as f:
    f.write("\n".join(lines) + "\n")
```

Then `bazel shutdown` so the new startup flags take effect.

`bazel build //...` still fails in the sandbox because `rules_go` fetches
`https://go.dev/dl/?mode=json` (proxy 403). CI has open network and is
unaffected. Locally, exercise the parts that don't pull in the runner:

- `bazel build //examples/hello:hello //examples/aws:s3_bucket //examples/gcp:storage_bucket`
- `bazel build //examples/hello:greet_lib //examples/aws:s3_bucket_lib //examples/gcp:storage_bucket_lib`
- `bazel test //examples/...:all` (fmt_check tests only)
- Runner unit tests outside Bazel: copy `main.go` + `main_test.go` to a
  scratch dir with a trivial `go.mod` and `go test ./...` (the canonical
  Bazel run is `bazel test //tf/private/cmd/runner:runner_test`,
  which needs network).

## Invariants — don't regress these

- No `.terraform.lock.hcl` is generated, shipped, or accepted in
  `srcs`/`data`. The per-platform `sha256` on each
  `tf_providers.provider(...)` tag (and the table in
  `toolchain/versions.bzl` for tofu itself) is the sole pinning layer:
  every download is gated on it and the repository cache keys on it.
  Both extensions declare `reproducible = True`, so they deliberately do
  *not* appear in `MODULE.bazel.lock` — the lockfile pins the registry
  module graph, not the provider/tofu artefacts.
  `init_action.bzl` fails loudly if a lock file slips into the work tree;
  don't relax that check.
- Both module extensions call `module_ctx.extension_metadata` with the
  dev/non-dev split keyed on `root_module_has_non_dev_dependency`, so
  `bazel mod tidy` can manage downstream `use_repo` lines. Root direct
  deps are `tofu_toolchains` only (never the internal `tofu_<platform>`
  repos) and, for providers, only the repos backing the **root module's
  own** tags. `bazel mod tidy` must leave `MODULE.bazel` unchanged.
- The tofu extension resolves `tofu.version` as "root arbitrates": >1
  root tag fails, one root tag wins unconditionally, agreeing non-root
  requests are honoured, disagreeing non-root requests fail naming the
  versions and modules, no tags means `DEFAULT_VERSION`. The branching
  lives in `toolchain/version_resolution.bzl` as a pure function that
  returns `struct(version, error)` — the extension owns the `fail()` so
  the logic stays unit-testable from `//toolchain/tests`. Don't fold it
  back inline, and don't make it last-write-wins. The providers
  extension stays "root wins, else highest version": a provider is a
  per-configuration artefact (MVS-shaped), the tofu binary is one global
  tool.
- `tf_library` validates inline — the validate stamp goes in
  `DefaultInfo.files` and `bazel build :foo_lib` is the validation
  contract. Don't add a `:foo.validate` sub-target or a `validate_test`
  macro flag. The only test target a `tf_library` generates is
  `:foo.fmt_check`.
- `_ALLOWED_EXTS` in `library.bzl` / `deploy.bzl` includes `.hcl`, which
  technically permits a user to list `.terraform.lock.hcl` in `srcs`.
  The runtime rejection in `init_action.bzl` is the backstop — keep it.
- The build-time validate action and the runtime runner both pick the
  **exec-platform** provider binary. Cross-compilation to a different
  target platform is out of scope and isn't worth designing around
  speculatively.
- The runner expects `--plugin-dir` to be required, and `os.MkdirAll`s
  it on first use so the zero-providers case still presents tofu with
  an extant directory. Don't make the flag optional.
- The runner refuses `apply`/`destroy` when no backend/cloud block is
  detected and `tf_deploy(allow_ephemeral_state = ...)` is unset: local
  state under `bazel-bin` is machine-local and `bazel clean`-able, so
  applying against it orphans live infra. The gate lives in the Go runner
  (backend detection is a runtime filesystem scan), fires before any tofu
  process starts or the state dir is created, and `plan` stays ungated.
  Don't downgrade it to a warning.
- Shared materialization helpers live in `tf/private/work_tree.bzl`
  (`materialize`, `materialize_plugin_tree`, `work_tree_root`,
  `plugin_tree_root`). Both `library.bzl` and `deploy.bzl` should go
  through them; don't reintroduce inline copies. The plugin tree is a
  sibling of the work tree (`<name>.plugins/`, not nested under
  `<name>.work/`), so the validate action's work-tree copy never touches
  provider binaries.
- `module()` in `MODULE.bazel` deliberately has no `version`: the release
  tag is the single source of truth, and publish-to-bcr (or BCR's
  `add_module` on the manual path) patches the real version into the
  registry entry. Don't add a version back, and don't hardcode release
  versions in README/docstring snippets — use the `X.Y.Z` placeholder
  plus a registry link.
- `bazel_skylib` is a `dev_dependency`, so the Starlark unit tests that
  load it live in the `//toolchain/tests` subpackage:
  `toolchain/BUILD.bazel` is loaded by every downstream module (it holds
  `:toolchain_type`) and must never reference a dev-only repo.
- The `.bazelversion` copies (root, `e2e/smoke`,
  `tests/integration/broken_workspace`,
  `tests/integration/broken_module`) must stay byte-identical: nested
  modules are separate bazelisk boundaries that never see the root pin.
  CI's lint job `cmp`s every nested copy against the root; bump them all
  together.
- The `--deleted_packages` lines in `.bazelrc` are owned by
  `bazel run @rules_bazel_integration_test//tools:update_deleted_packages`,
  which rewrites *every* non-comment `--deleted_packages` line with an
  auto-discovered list covering all nested workspaces (including
  `e2e/smoke`). Rerun it after adding/removing nested packages; don't
  hand-edit the lists or add separate one-off lines.
- The release tarball (`release_prep.sh`) excludes `examples/`, `tests/`
  and `toolchain/tests/` but must keep `e2e/` and `.bcr/` — BCR presubmit
  reads the test module and templates from the extracted archive. The
  pathspecs are prefix-anchored, so a package named `tests` nested under a
  shipped directory needs its own exclusion; anything loading a
  `dev_dependency` must not ship.

## Branch / PR conventions

Development branch (`claude/...`); push there, not `main`. Don't open a
PR unless asked. Don't include the model identifier (`claude-opus-4-7`
etc.) in any commit message, PR title, or code artifact.
