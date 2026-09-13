# Negative integration tests

Bazel-in-Bazel tests that assert misuses of `tf_library` /
`tf_deploy` fail with the error messages the rules promise.

## Layout

```
tests/integration/
├── BUILD.bazel              bazel_integration_test target per case
├── assert_bazel_fails.sh    runs the nested bazel (build or run) and
│                            asserts non-zero exit + stderr fragment
└── broken_workspace/        sub-workspace for the nested bazel
    ├── MODULE.bazel
    ├── .bazelversion
    └── cases/
        ├── auto_tfvars_conflict/{BUILD.bazel, main.tf, rules_tofu.auto.tfvars.json}
        ├── conflicting_providers/{BUILD.bazel, main.tf}
        ├── duplicate_vars/{BUILD.bazel, main.tf, extra.tfvars.json}
        ├── ephemeral_state_refused/{BUILD.bazel, main.tf}
        ├── external_module_ref/{BUILD.bazel, main.tf}
        ├── lock_file_smuggled/{BUILD.bazel, main.tf, .terraform.lock.hcl}
        ├── malformed_var_file/{BUILD.bazel, main.tf, bad.tfvars.json}
        ├── missing_provider/{BUILD.bazel, main.tf}
        └── validate_fails/{BUILD.bazel, main.tf}
```

The child workspace packages are excluded from the parent Bazel via
the `--deleted_packages` flag in `.bazelrc` (the "deleted packages"
trick recommended by `rules_bazel_integration_test`), so the outer
Bazel never analyzes the intentionally-failing case targets when
expanding `//...`.

## Case lists

`BUILD.bazel` holds two lists, distinguished only by the nested Bazel
command the driver runs:

- `_CASES` — build-mode cases. The failure comes from loading,
  analysis, or one of the actions the rules emit, so `bazel build` on
  the case target is enough to surface it. This is the driver's
  default, so these rows set no `BAZEL_COMMAND`.
- `_RUN_CASES` — run-mode cases, with `BAZEL_COMMAND: "run"` in `env`.
  The case target builds cleanly and the refusal comes from the
  launcher process a `tf_deploy` generates. These are the only tests
  that exercise the path from a BUILD attribute through the generated
  launcher to a runner flag; the runner's own Go unit tests pass flags
  directly and cannot see that plumbing.

## Running

```sh
bazel test //tests/integration:all
```

Each test spawns a nested Bazel that materializes the case's work tree
and runs `tofu init/validate` (or the dupcheck action), or — for a
run-mode case — launches the deploy's runner; the driver checks the
failure mode.

## Adding a case

1. Create `broken_workspace/cases/<case>/` with a `BUILD.bazel` that
   misuses the rules, plus any fixture files the failure mode needs.
2. Add a row to `tests/integration/BUILD.bazel`:
   `("<case>", "//cases/<case>:<target>", "<stderr-fragment>")`. Put it
   in `_CASES` if `bazel build` on the target fails, or in `_RUN_CASES`
   if the target builds and only `bazel run` fails — the latter
   comprehension passes `BAZEL_COMMAND=run` to the driver.
3. Execute `bazel run @rules_bazel_integration_test//tools:update_deleted_packages`
   to add the new package to `--deleted_packages` in the root `.bazelrc`.

Pick a stderr fragment short and stable enough not to break on tofu /
rule-error wording tweaks; avoid quoting full sentences.

## Follow-ups

All core build-time negative failure modes are covered, as is one
runtime mode: the ephemeral-state refusal on `bazel run :deploy.apply`
when the deploy has no backend and has not opted in.

Still untested end-to-end: the opt-in direction (a deploy that sets
`allow_ephemeral_state = True`, or declares a backend, reaching tofu),
`bazel run :deploy.plan` outside the Bazel environment, and
`tf_providers` module-resolution errors. These can be addressed in
future testing passes if needed.
