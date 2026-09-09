#!/usr/bin/env python3
"""Personal-only Linux PC1 bootstrap/build provenance. Never installs Curio."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import signal
import subprocess
import sys
import time

PROFILES = {
    "sdisk-4slot": {"path": "/sdisk/sealworker", "capacity": 5600000000000},
    "sdisk-sm": {"path": "/sdisk_sm/sealworker", "capacity": 16800000000000},
}
REQUESTED = {"FFI_USE_CUDA": "1", "FFI_USE_CUDA_SUPRASEAL": "0",
             "FFI_BUILD_FROM_SOURCE": "1", "RUSTFLAGS": "-C target-cpu=native -g"}
MAKE_VARS = ["CURIO_TAGS", "FFI_USE_CUDA", "FFI_USE_OPENCL",
             "FFI_USE_CUDA_SUPRASEAL_EFFECTIVE", "DISABLE_SUPRASEAL", "CC", "CXX"]


def digest(path):
    with open(path, "rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def clean_env(inherited):
    # Deliberately do not inherit DB, Curio, integration, Go/FFI overrides,
    # credential, proxy, Python package-index or service configuration.
    env = {key: inherited[key] for key in ("PATH", "HOME", "TMPDIR") if key in inherited}
    env.update(REQUESTED)
    env.update(FFI_USE_OPENCL="0", DISABLE_SUPRASEAL="0", CGO_ENABLED="1",
               CGO_LDFLAGS_ALLOW=".*", GOENV="off", GOTOOLCHAIN="local", LANG="C.UTF-8")
    return env


def run(argv, cwd, env, log=None, timeout=120):
    # No shell/tee success masking. Kill and join the owned process group on
    # timeout/interruption, rather than leaving a native build running.
    started = time.monotonic()
    with subprocess.Popen(argv, cwd=cwd, env=env, stdout=log or subprocess.PIPE,
                          stderr=subprocess.STDOUT, text=True, start_new_session=True) as proc:
        try:
            output, _ = proc.communicate(timeout=timeout)
        except BaseException:
            os.killpg(proc.pid, signal.SIGTERM)
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(proc.pid, signal.SIGKILL)
                proc.wait()
            raise
        if log:
            log.write(f"\ncommand={argv!r} exit={proc.returncode} seconds={time.monotonic()-started:.3f}\n")
            log.flush()
        if proc.returncode:
            raise RuntimeError(f"command failed ({proc.returncode}): {argv!r}; see build log")
        return (output or "").strip()


def source_identity(root, env):
    def git(*args):
        return run(["git", *args], root, env)
    if git("status", "--porcelain", "--untracked-files=no"):
        raise RuntimeError("tracked source/submodule state is dirty")
    pins = {}
    for line in git("ls-tree", "-r", "HEAD").splitlines():
        if line.startswith("160000 "):
            meta, name = line.split("\t")
            pins[name] = meta.split()[2]
            actual = run(["git", "rev-parse", "HEAD"], root / name, env)
            if actual != pins[name] or run(["git", "status", "--porcelain", "--untracked-files=no"], root / name, env):
                raise RuntimeError(f"uninitialized, mismatched or dirty submodule: {name}")
    inputs = ["Makefile", "scripts/build-blst.sh"]
    inputs += [p for p in git("ls-files", "scripts/makefiles", "extern/supraseal").splitlines()
               if (root / p).is_file()]
    return {"head": git("rev-parse", "HEAD"), "tree": git("rev-parse", "HEAD^{tree}"),
            "submodules": pins, "build_inputs": {p: digest(root / p) for p in inputs}}


def make_settings(root, env, output):
    # This additional target only prints expanded variables; unlike `make -n
    # all`, it cannot run recursive native recipes. No tracked file is edited.
    query = output / "make-settings.mk"
    query.write_text(".PHONY: personal-settings\npersonal-settings:\n" + "".join(
        f"\t@printf '%s\\n' '{key}=$({key})'\n" for key in MAKE_VARS))
    lines = run(["make", "--no-print-directory", "-f", "Makefile", "-f", str(query),
                 "personal-settings"], root, env).splitlines()
    settings = dict(line.split("=", 1) for line in lines if "=" in line)
    expected = {"CURIO_TAGS": "cunative", "FFI_USE_CUDA": "1", "FFI_USE_OPENCL": "0",
                "FFI_USE_CUDA_SUPRASEAL_EFFECTIVE": "0", "DISABLE_SUPRASEAL": "0"}
    if any(settings.get(k) != v for k, v in expected.items()):
        raise RuntimeError(f"unexpected Makefile settings: {settings!r}")
    return settings


def native_artifacts(root):
    required = ["extern/filecoin-ffi/libfilcrypto.a", "extern/filecoin-ffi/filcrypto.h",
                "extern/filecoin-ffi/filcrypto.pc", "extern/supraseal/obj/libsupraseal.a",
                "extern/supraseal/deps/blst/libblst.a"]
    for name in required:
        if not (root / name).is_file():
            raise RuntimeError(f"missing native artifact: {name}")
    paths = set(required)
    # Only link inputs, not every intermediate Rust object/cache in the tree.
    for name in ("extern/supraseal/deps/spdk-v24.05/build/lib",
                 "extern/supraseal/deps/spdk-v24.05/dpdk/build/lib"):
        directory = root / name
        if not directory.is_dir():
            raise RuntimeError(f"missing native link directory: {name}")
        paths.update(str(p.relative_to(root)) for p in directory.iterdir()
                     if p.is_file() and (p.suffix == ".a" or ".so" in p.name))
    return {p: digest(root / p) for p in sorted(paths)}


def native_sources(root, env):
    result = {}
    for name in ("extern/supraseal/deps/blst", "extern/supraseal/deps/spdk-v24.05"):
        directory = root / name
        if not directory.is_dir() or Path(run(["git", "rev-parse", "--show-toplevel"], directory, env)).resolve() != directory.resolve():
            raise RuntimeError(f"native dependency source is not identifiable: {name}")
        result[name] = {"head": run(["git", "rev-parse", "HEAD"], directory, env),
                        "tracked_diff_sha256": hashlib.sha256(run(["git", "diff", "HEAD"], directory, env).encode()).hexdigest(),
                        "submodule_status": run(["git", "submodule", "status", "--recursive"], directory, env)}
    return result


def ffi_evidence(log):
    lines = log.splitlines()
    selection = "Using additional build flags: --no-default-features --features multicore-sdr,cuda,fvm"
    if selection not in lines:
        raise RuntimeError("native log does not establish the required CUDA/multicore/FVM feature selection")
    rust = [line for line in lines if line.startswith("+ RUSTFLAGS=") and "native-static-libs" in line]
    if not rust:
        raise RuntimeError("native log lacks effective Rust flags; do not infer them from requested flags")
    return {"feature_evidence": selection, "rust_flag_evidence": sorted(set(rust))}


def cpu_flags():
    # Native Rust/SupraSeal objects are machine-ISA dependent. No hostname is
    # recorded; require the same CPU feature set when reusing a receipt.
    return sorted(next(line.split(":", 1)[1].split() for line in Path("/proc/cpuinfo").read_text().splitlines()
                       if line.startswith("flags")))


def validate_receipt(receipt, source, artifacts, flags, settings):
    expected = {"submodules": source["submodules"], "build_inputs": source["build_inputs"],
                "artifacts": artifacts, "cpu_flags": flags, "requested": REQUESTED,
                "make_settings": settings, "platform": "Linux-x86_64"}
    if receipt.get("native_build") != "PASS" or any(receipt.get(k) != v for k, v in expected.items()):
        raise RuntimeError("native receipt is missing/incompatible; perform separate bootstrap, not a fallback build")


def verify_binary(metadata, head):
    for expected in (f"vcs.revision={head}", "vcs.modified=false", "GOOS=linux", "GOARCH=amd64", "GOAMD64=v3"):
        if expected not in metadata.split():
            raise RuntimeError(f"binary provenance mismatch: {expected}")
    if '-tags=cunative' not in metadata.split():
        raise RuntimeError("binary tags are not the requested cunative build")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("plan", "bootstrap", "build"))
    parser.add_argument("--profile", choices=PROFILES, required=True)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--head", required=True)
    parser.add_argument("--tree", required=True)
    parser.add_argument("--output", type=Path, required=True, help="new directory outside the source checkout")
    parser.add_argument("--native-receipt", type=Path)
    parser.add_argument("--allow-native-bootstrap", action="store_true")
    args = parser.parse_args()
    root, output = args.root.resolve(), args.output.resolve()
    if output.is_relative_to(root):
        raise RuntimeError("output must be outside the source checkout")
    if args.mode != "plan" and (platform.system(), platform.machine()) != ("Linux", "x86_64"):
        raise RuntimeError("Linux x86_64 required; Darwin artifacts cannot be used")
    env = clean_env(os.environ)
    source = source_identity(root, env)
    if (source["head"], source["tree"]) != (args.head, args.tree):
        raise RuntimeError("source does not match the explicit expected head/tree")
    output.mkdir(parents=True, exist_ok=False)
    manifest = {"source": source, "profile": args.profile, "virtual_capacity": PROFILES[args.profile],
                "runtime_environment": {"CURIO_PERSONAL_STORAGE_PROFILE": args.profile},
                "requested": REQUESTED, "linux_effective_build": "NOT_RUN", "binary_sha256": None}
    if args.mode == "plan":
        (output / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
        return
    for tool in ("go", "make", "nvcc", "gcc", "g++", "rustc", "cargo", "rustup", "jq", "pkg-config", "python3"):
        if not shutil.which(tool, path=env.get("PATH")):
            raise RuntimeError(f"missing prerequisite {tool}; no backend substitution or automatic OS install")
    settings = make_settings(root, env, output)
    flags = cpu_flags()
    manifest.update(make_settings=settings, cpu_flags=flags)
    installed = Path("/usr/local/bin/curio")
    before = digest(installed) if installed.exists() else None
    manifest["installed_sha256_before"] = before
    try:
        with (output / "build.log").open("w", buffering=1) as log:
            for argv in (["go", "version"], ["go", "env", "GOOS", "GOARCH", "CGO_ENABLED"],
                         ["rustc", "-vV"], ["cargo", "--version"], ["nvcc", "--version"],
                         [settings["CC"], "--version"], [settings["CXX"], "--version"]):
                run(argv, root, env, log)
            if args.mode == "bootstrap":
                if not args.allow_native_bootstrap:
                    raise RuntimeError("bootstrap requires --allow-native-bootstrap; it can fetch native build dependencies")
                # Submodules already validated. Do not invoke update-modules,
                # setup-cgo-env, clean, install, or mutate build sources.
                for argv in (["make", "-B", "curio-libfilecoin"], ["bash", "scripts/build-blst.sh"],
                             ["bash", "-c", "cd extern/supraseal && exec ./build.sh"]):
                    run(argv, root, env, log, timeout=14400)
                receipt = {"native_build": "PASS", "platform": "Linux-x86_64", "requested": REQUESTED,
                           "submodules": source["submodules"], "build_inputs": source["build_inputs"],
                           "artifacts": native_artifacts(root), "cpu_flags": flags, "make_settings": settings,
                           "native_sources": native_sources(root, env),
                           "effective_ffi": ffi_evidence((output / "build.log").read_text()),
                           "log_sha256": digest(output / "build.log")}
                (output / "native-receipt.json").write_text(json.dumps(receipt, indent=2) + "\n")
                manifest["linux_effective_build"] = "NATIVE_BOOTSTRAP_ONLY"
            else:
                if not args.native_receipt:
                    raise RuntimeError("ordinary build requires --native-receipt from the separate bootstrap")
                receipt = json.loads(args.native_receipt.read_text())
                validate_receipt(receipt, source, native_artifacts(root), flags, settings)
                if receipt.get("native_sources") != native_sources(root, env) or not receipt.get("effective_ffi"):
                    raise RuntimeError("native source/effective-feature evidence changed or is missing")
                manifest["native_receipt_sha256"] = digest(args.native_receipt)
                manifest["native_sources"] = receipt["native_sources"]
                manifest["effective_ffi"] = receipt["effective_ffi"]
                for name in ("curio", "sptool"):
                    if (root / name).is_file():
                        shutil.copy2(root / name, output / (name + ".before"))
                run(["make", "ffi-version-check"], root, env, log)
                # The normal repository target and flags are preserved. Only
                # already-verified native prerequisites are skipped.
                run(["make", "all", "BUILD_DEPS="], root, env, log, timeout=7200)
                binary = root / "curio"
                metadata = run(["go", "version", "-m", str(binary)], root, env)
                (output / "go-version-m.txt").write_text(metadata + "\n")
                verify_binary(metadata, args.head)
                file_type = run(["file", str(binary)], root, env)
                if "ELF 64-bit" not in file_type or "x86-64" not in file_type:
                    raise RuntimeError("binary is not Linux x86-64 ELF")
                (output / "file.txt").write_text(file_type + "\n")
                (output / "ldd.txt").write_text(run(["ldd", str(binary)], root, env) + "\n")
                (output / "version.txt").write_text(run([str(binary), "--version"], root, env) + "\n")
                shutil.copy2(binary, output / "curio")
                manifest.update(binary_sha256=digest(binary))
            if source_identity(root, env) != source:
                raise RuntimeError("build changed tracked source or dependency identities")
            if args.mode == "build":
                manifest["linux_effective_build"] = "PASS"
    except BaseException:
        manifest["linux_effective_build"] = "FAIL"
        raise
    finally:
        after = digest(installed) if installed.exists() else None
        manifest["installed_sha256_after"] = after
        if after != before:
            manifest["linux_effective_build"] = "FAIL"
        (output / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
        if after != before:
            raise RuntimeError("installed binary changed during build; investigate, do not deploy")


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, OSError, subprocess.TimeoutExpired, ValueError) as error:
        print(f"BUILD BLOCKED: {error}", file=sys.stderr)
        sys.exit(1)
