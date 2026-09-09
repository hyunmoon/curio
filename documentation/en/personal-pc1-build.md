# Personal PC1 source and Linux build handoff

This is PERSONAL_ONLY tooling. It neither installs a binary nor modifies a
service, configuration layer, queue or database. The two runtime capacity
profiles use **one source tree and one binary implementation**. A build profile
in the manifest does not configure the running process: the operator must
separately approve its eventual service environment. No binary in this topic is
approved for deployment to other Curio roles.

## Requested versus effective build contract

The requested command for both profiles is:

```sh
env FFI_USE_CUDA=1 FFI_USE_CUDA_SUPRASEAL=0 FFI_BUILD_FROM_SOURCE=1 \
  RUSTFLAGS="-C target-cpu=native -g" make all
```

The deployed-base and pinned integration-base Makefile fragments have the same
relevant contents. Their actual contract is:

| Layer | Selection / evidence |
| --- | --- |
| Go target | `make all` builds `curio` and `sptool`. `curio` explicitly uses `GOAMD64=v3`; it is not `curio-native`. |
| Go tags | With OpenCL off and `DISABLE_SUPRASEAL=0`, `CURIO_TAGS=cunative`. Do not use the Darwin validation tags as Linux build flags. |
| Filecoin FFI | `curio-libfilecoin` requests GPU, CUDA, multicore SDR, source build, and CUDA-SupraSeal **off**. Pinned FFI adds FVM by default. Expected features: `multicore-sdr,cuda,fvm`, to be confirmed in the native log. |
| Rust | The Curio recipe overrides the requested flags with `-C codegen-units=1 -C opt-level=3 -C strip=symbols`. Pinned `install-filcrypto` can replace those with detected `-C target-feature=...`; `build-release.sh` prefixes `--print native-static-libs`. Record the actual logged flags, not the outer environment as an effective result. |
| Curio SupraSeal | **Enabled.** `FFI_USE_CUDA_SUPRASEAL=0` is an FFI feature, not `DISABLE_SUPRASEAL=1`. Both profiles use the same selection. |
| SupraSeal CPU/compiler | Its own build defaults to `-march=native` and chooses GCC 12/13 independently of the root Makefile's selection. SPDK/DPDK/native-library settings remain those in the pinned script. |
| Native portability | Go requires x86-64-v3; native Rust/SupraSeal may require additional build-host ISA features. A successful local build is not proof that every worker supports them. |

Exact submodule commits are read from the selected Git tree. Never substitute a
Darwin archive. A clean source tree cannot by itself identify an old archive's
backend; absence of a compatible native receipt is a build gate, not permission
to guess. The original generic deployed build's OpenCL metadata is **not**
evidence of a CUDA PC1 archive.

## Separate native bootstrap (operator only, not executed by this task)

Use an existing trusted Linux x86_64 build environment with the pinned Go and
Rust toolchains, CUDA/nvcc, suitable GCC/G++, jq, pkg-config, and the OS native
development packages required by `extern/supraseal/README.md` and `build.sh`.
Python needs `venv`, pip, setuptools and wheel; distributions splitting venv by
Python minor version also need that matching venv package. Package installation
is a separate machine-administration action, not part of an ordinary build.

Initialize/verify the exact Git submodule pins before invoking this tool.
The bootstrap intentionally calls the repository's native scripts; SupraSeal's
script may create its `.venv` and install/update its own Python build tools.
This behavior is confined to bootstrap. Do not add it to every build, and do not
run `make clean` or source-patching commands.

The existing BLST bootstrap script can clone a moving default branch when its
directory is absent; this topic does not silently change that native dependency
policy. Prefer the existing trusted native source checkout. The receipt records
the actual BLST/SPDK revisions, tracked-diff hashes and recursive submodule
status alongside the two top-level Git pins. A fresh bootstrap is therefore
not claimed to be bit-reproducible from the Curio SHA alone.

From the selected, clean source checkout, with full `HEAD`/tree copied from the
reviewed handoff manifest and a **new output directory outside the checkout**:

```sh
python3 scripts/personal/pc1_build.py bootstrap \
  --profile sdisk-4slot --head "$EXPECTED_HEAD" --tree "$EXPECTED_TREE" \
  --output "$NEW_NATIVE_OUTPUT" --allow-native-bootstrap
```

Use `--profile sdisk-sm` for its named manifest if desired. Native selection is
identical. The tool logs each command, checks its exit status, terminates and
joins its own process group on timeout, and writes a receipt with native
artifact hashes, source-build inputs, pins and CPU features. Read that log and
record the effective Rust/FFI/SupraSeal fields in
`scripts/personal/build-manifest.template.json`; unconfirmed fields remain
UNVERIFIED. The logged native command is evidence; requested flags alone are not.
No bootstrap has been executed by the assistant on Linux.

## Ordinary build: two explicit wrappers

The same clean checkout can build either profile. Supply a verified receipt
from the separate bootstrap and use a fresh output directory for each run:

```sh
bash scripts/personal/build-sdisk-4slot.sh \
  --head "$EXPECTED_HEAD" --tree "$EXPECTED_TREE" \
  --native-receipt "$NATIVE_OUTPUT/native-receipt.json" --output "$NEW_BUILD_OUTPUT"
```

```sh
bash scripts/personal/build-sdisk-sm.sh \
  --head "$EXPECTED_HEAD" --tree "$EXPECTED_TREE" \
  --native-receipt "$NATIVE_OUTPUT/native-receipt.json" --output "$NEW_BUILD_OUTPUT"
```

The ordinary build validates all pins, build-input and native hashes, CPU
features and expanded Makefile settings before running `make ffi-version-check`
then **`make all BUILD_DEPS=`**. Only verified native prerequisites are skipped;
the normal target, CPU selection and build tags are unchanged. It does not
apt-install, pip-upgrade, run `make clean`, rebuild native libraries, change
tracked source or use `make install`. DB/service/credential/build overrides are
not inherited. Its allowlisted environment disables persistent Go env overrides.

The output preserves the prior build-tree binaries if present, the command log,
source/tree/submodule/profile manifest, native receipt hash, Go version-m data,
ELF file type, dynamic dependencies, version output and final binary SHA-256.
It checks Linux/amd64, `GOAMD64=v3`, `cunative`, the exact VCS revision and
`vcs.modified=false`, and verifies `/usr/local/bin/curio` has not changed. A
nonzero exit is a failed build even when a log exists. No deployment commands
are provided. Preserve custom role-specific binaries until a separate rollout
approval; this handoff is only the reviewed PC1 lineage.

At runtime the selected local profile is
`CURIO_PERSONAL_STORAGE_PROFILE=sdisk-4slot` or `sdisk-sm`. Unset means ordinary
upstream storage accounting. The active personal jitter configurations are
documented separately; build-time environment does not apply them to a service.
