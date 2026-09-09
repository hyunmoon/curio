"""Database-free checks of the actual personal build guard/tooling functions."""

import json
import os
from pathlib import Path
import sys
import subprocess
import tempfile
import unittest

import pc1_build as build


class PC1BuildTests(unittest.TestCase):
    def test_profiles_and_environment_are_explicit(self):
        self.assertEqual(build.PROFILES, {
            "sdisk-4slot": {"path": "/sdisk/sealworker", "capacity": 5600000000000},
            "sdisk-sm": {"path": "/sdisk_sm/sealworker", "capacity": 16800000000000}})
        env = build.clean_env({"PATH": os.defpath, "HOME": "/synthetic", "PGHOST": "forbidden",
                               "CURIO_HARMONYDB_HOSTS": "forbidden", "CURIO_MK20_RELEASE_ITEST": "1",
                               "FFI_USE_OPENCL": "1", "GOFLAGS": "-tags=other", "GITHUB_TOKEN": "synthetic"})
        self.assertNotIn("PGHOST", env)
        self.assertNotIn("CURIO_HARMONYDB_HOSTS", env)
        self.assertNotIn("CURIO_MK20_RELEASE_ITEST", env)
        self.assertNotIn("GITHUB_TOKEN", env)
        self.assertNotIn("GOFLAGS", env)
        self.assertEqual(env["GOENV"], "off")
        self.assertEqual(env["FFI_USE_OPENCL"], "0")
        self.assertEqual(env["FFI_USE_CUDA_SUPRASEAL"], "0")
        self.assertEqual(env["DISABLE_SUPRASEAL"], "0")
        self.assertEqual(env["RUSTFLAGS"], "-C target-cpu=native -g")

    def test_receipt_rejects_wrong_backend_source_artifact_cpu(self):
        source = {"submodules": {"ffi": "pin"}, "build_inputs": {"make": "hash"}}
        artifacts, flags, settings = {"ffi.a": "hash"}, ["avx2"], {"CURIO_TAGS": "cunative"}
        receipt = dict(source, native_build="PASS", platform="Linux-x86_64", requested=build.REQUESTED,
                       artifacts=artifacts, cpu_flags=flags, make_settings=settings)
        build.validate_receipt(receipt, source, artifacts, flags, settings)
        for key in receipt:
            bad = dict(receipt, **{key: "wrong"})
            with self.subTest(key=key), self.assertRaisesRegex(RuntimeError, "incompatible"):
                build.validate_receipt(bad, source, artifacts, flags, settings)

    def test_binary_identity(self):
        metadata = "vcs.revision=abc vcs.modified=false GOOS=linux GOARCH=amd64 GOAMD64=v3 -tags=cunative"
        build.verify_binary(metadata, "abc")
        for value in ("vcs.revision=abc", "vcs.modified=false", "GOOS=linux", "GOARCH=amd64", "GOAMD64=v3", "-tags=cunative"):
            with self.subTest(value=value), self.assertRaises(RuntimeError):
                build.verify_binary(metadata.replace(value, "wrong"), "abc")

    def test_command_failure_is_not_tee_success(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with (root / "build.log").open("w") as log, self.assertRaises(RuntimeError):
                build.run([sys.executable, "-c", "print('synthetic failure'); raise SystemExit(7)"],
                          root, build.clean_env(os.environ), log)
            self.assertIn("exit=7", (root / "build.log").read_text())

    def test_receipt_json_round_trip(self):
        self.assertEqual(json.loads(json.dumps(build.REQUESTED)), build.REQUESTED)

    def test_timeout_joins_owned_child(self):
        with tempfile.TemporaryDirectory() as directory, self.assertRaises(subprocess.TimeoutExpired):
            build.run([sys.executable, "-c", "import time; time.sleep(30)"],
                      Path(directory), build.clean_env(os.environ), timeout=0.05)

    def test_native_evidence_does_not_infer_requested_flags(self):
        selection = "Using additional build flags: --no-default-features --features multicore-sdr,cuda,fvm"
        actual = "+ RUSTFLAGS='--print native-static-libs -C target-feature=+avx2'"
        evidence = build.ffi_evidence(selection + "\n" + actual)
        self.assertEqual(evidence["rust_flag_evidence"], [actual])
        for missing in (selection, actual, selection.replace(",cuda,", ",opencl,") + "\n" + actual):
            with self.assertRaises(RuntimeError):
                build.ffi_evidence(missing)


if __name__ == "__main__":
    unittest.main()
