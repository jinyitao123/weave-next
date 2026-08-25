# RFC 8785 Fixture Provenance

Retrieved: 2026-07-24

## Normative source

- RFC: RFC 8785, JSON Canonicalization Scheme (JCS)
- RFC Editor URL: https://www.rfc-editor.org/rfc/rfc8785.txt
- IETF mirror URL: https://www.ietf.org/rfc/rfc8785.txt
- Retrieved byte length: 41879
- SHA-256: `63d52294eb0e3f0014174288186d388b4ddbf2c67d1ce8af1d9726eb0c3ab240`
- Verification: the RFC Editor and IETF mirror downloads were byte-for-byte
  identical on the retrieval date.

The fixtures cover RFC 8785 Section 3.2.3 object-property sorting and the
finite IEEE-754 examples from Appendix B. Appendix A is an ECMAScript
reference implementation, not an input/output vector, so it is intentionally
not transcribed or reinterpreted as a test vector.

## Fixture source and checksums

The files below are byte-for-byte copies from the official RFC 8785 fixture
directory in the Go Proxy artifact for
`github.com/lattice-substrate/json-canon@v0.3.4`:

`conformance/official/rfc8785/`

| File | RFC coverage | SHA-256 |
| --- | --- | --- |
| `appendix_b.csv` | Appendix B finite number serialization samples | `2fc8b1c494e7a22cce3f7875c7b64ebd0e071632e2d23aeeceb3d80f972d05a0` |
| `key_sorting_input.json` | Section 3.2.3 input | `0f7291c1b0a21d3c7b1c8571743571b190d6aa5ca532f8d945c562c517a051aa` |
| `upstream_key_sorting_output_known_bad.json` | Upstream's incorrect claimed Section 3.2.3 output, retained as evidence only | `8ef85608be985aef492df078c308b9b36cd5f0372db8428136c5394e5a46e9ad` |

No fixture values, ordering, spelling, whitespace, or encoding were changed.
The upstream output file was renamed to prevent its use as a golden value.
Appendix B rows for NaN and Infinity are absent because they are not finite
JSON numbers and the upstream fixture is explicitly the finite-number subset.

### Known-bad upstream output evidence

In `github.com/lattice-substrate/json-canon@v0.3.4`, the upstream README claims
that `key_sorting_output.json` is the Section 3.2.3 output. Its first six bytes
are `7b 22 5c 5c 72 22`, so JSON decoding produces a two-character property
name consisting of backslash plus `r`. RFC 8785 Section 3.2.3 instead defines
the property name as U+000D Carriage Return, represented in JSON by bytes
`22 5c 72 22`.

The same upstream version's `conformance/official_suites_test.go` does not read
this output file. It hardcodes the correct RFC value and order instead. The
file is therefore retained byte-for-byte under the explicit `known_bad` name
for provenance and regression detection only. It must never be used as the
normative expected canonical output.

## Go dependency lock

- Module: `github.com/lattice-substrate/json-canon`
- Version: `v0.3.4`
- Import used by the next implementation task:
  `github.com/lattice-substrate/json-canon/jcs`
- Authoritative proxy: `https://proxy.golang.org`
- SumDB: `sum.golang.org`
- Module sum:
  `h1:Zf1dyp6ac+hMYLeAmR24DS+0+JkLORp5VXfKORJIfRo=`
- go.mod sum:
  `h1:3fIkLCuIYfANsL1ezLEW0Is2jZ7vEhoxe6lRMTIy9ZI=`
- Proxy zip SHA-256:
  `30ea7570a97b4abe9345e9b4486ac4819ffbec83fdce9fa75de1a975f44d61f5`
- Proxy origin ref: `refs/tags/v0.3.4`
- Proxy origin commit:
  `a768d98908f27c940aebff53265586502f059f48`

The version was downloaded with a single proxy URL and SumDB enabled. Direct
VCS fallback is forbidden for this lock.

### Mutable upstream tag warning

On 2026-07-24, the GitHub API reported `v0.3.4` as annotated tag object
`5079be3942b60392d376bc50520549ccd2baa1aa`, peeled to unsigned commit
`10154ec1f79c4b3fff771316eac8e344f7a94b1c`. This differs from the immutable
origin commit recorded by the Go Proxy.

The approved preflight comparison reported 73/73 tree divergence while the
required core package blobs were content-identical. This task therefore pins
the Go Proxy artifact and SumDB checksums as authoritative, vendors only the
required core packages, and does not resolve or update from the current
GitHub tag.

## TypeScript dependency lock

- Package: `canonicalize`
- Version: `3.0.0` (exact, no range)
- Registry: `https://registry.npmjs.org`
- Tarball:
  `https://registry.npmjs.org/canonicalize/-/canonicalize-3.0.0.tgz`
- Integrity:
  `sha512-yYLfHyDMIXRyRqsKBRLX023riFLpXY2YOfdtqKXZRZy9qsfOJ9U+4F9YZL7MEzL5+ziN2x2nlBvY/Voi3EBljA==`
- SHA-1 shasum: `a8073c48c1835631e1829d759078d2bbf48487fc`
- License: Apache-2.0
- Engine: Node.js `>=18`
- Runtime dependencies: none

The package is a development dependency used by the later cross-language
golden-vector task. No TypeScript canonicalization implementation is added by
this dependency-lock task.
