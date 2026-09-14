# Q5 causal reproduction materials

Reproduction materials for [filecoin-project/curio#1533](https://github.com/filecoin-project/curio/pull/1533).

[Download curio-q5-reproduction.zip](./curio-q5-reproduction.zip?raw=true).

SHA-256:

```text
8d7ee4c5daf3caa880cfbd41302fb025326af57f3f9634ac3eb1c31a0ab4af2c  curio-q5-reproduction.zip
```

The package compares:

- Upstream base: `86e95967cd602dbf8db5ea538abe986c3968e2e3`
- Reviewed candidate: `0c1f7cbd4dc9797e9773f1edd165af6b72d3fe27`

It contains test overlays, reproduction commands, recorded PostgreSQL results,
source excerpts, and checksums. Start with the package's `README.md`,
`SCENARIOS.md`, and `RESULTS.md`. `tests/` contains the exact executed overlays;
`formatted-tests/` contains formatting-only review copies.

The reproductions exercise real task and SQL paths in a disposable PostgreSQL
database, with native work substituted and the SDR success-write block extracted
unchanged. They distinguish assertion failures from setup failures. Retry-clock
and stale-completion regressions fail on the base and pass on the candidate.
The SDR filtering result is a limited admission improvement: without pacing,
the base still progresses on its next pass. These tests do not establish the
cause of a production incident, native termination, or stage-write fencing.

This publication only adds the reviewed ZIP and this README. It does not change
the PR's product code, rerun tests, or narrow the PR's implementation scope.
