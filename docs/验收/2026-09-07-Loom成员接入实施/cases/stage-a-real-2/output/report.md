# Corona Pre-Phase A — Frozen Baseline Validation Report

## 1. Purpose
Reproduce and independently verify the reference quantities of the frozen
engineering baseline for the single approved cruise scenario. Deliverables:
model.py, results.json, test_model.py and this report. baseline_frozen.yaml is
the sole input and was preserved unchanged throughout.

## 2. Baseline identity
- baseline_id: corona-baseline-1.0.0
- SHA-256: cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2
- Size: 8016 bytes
- The hash was re-verified before and after every execution; the file is
  byte-identical to the frozen original, and results.json carries the same
  digest under baseline_sha256.

## 3. Single approved speed
The baseline approves exactly one cruise speed: 0.03c = 8,993,773.74 m/s. The
0.01c and 0.05c variants are parked under pending change CCR-001 and are not
part of this baseline; no parked value was used anywhere in the computation.

## 4. Method and units
- Kinetic energy (classical): E = 1/2 m v^2 with m in kg and v in m/s, giving
  joules (1 J = 1 kg m^2 s^-2).
- Cruise travel time: t = d / v with d = 4.25 ly. One light-year is c times one
  Julian year (365.25 d x 86 400 s), so t in years reduces to 4.25 / 0.03.
- Habitat spin gravity: a = omega^2 r, with omega = 2 rpm x 2pi/60 rad/s and
  r = 1000 m, giving m/s^2 (g0 = 9.80665 m/s^2 used only for context).

## 5. Results (results.json, written by the verified model.py run)
| key | value | meaning |
|---|---|---|
| energy_1mt_j | 4.0443983043e+22 J | crewed_1mt: m = 1e9 kg at 0.03c |
| energy_5mt_j | 2.0221991522e+23 J | orbital_material_5mt: m = 5e9 kg (exactly 5x the 1 Mt case) |
| dust_j | 4.0443983043e+07 J | 1 mg grain at 0.03c relative speed (about 9.7 kg TNT equivalent) |
| travel_years | 141.6666666667 yr | Proxima cruise at 0.03c, Earth frame |
| gravity_m_s2 | 43.8649084493 m/s^2 | 1000 m habitat at 2 rpm (about 4.47 g0) |
| baseline_sha256 | cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2 | digest of the untouched input file |

## 6. Verification
test_model.py re-derives every quantity from first principles with all
constants re-entered independently of model.py, validates the results.json
schema and value types, re-hashes the input file, and cross-checks all five
quantities against the baseline's own reference_checks within their stated
relative tolerances (travel_years 0.001; energies, dust and gravity 0.01).
Final runs: model.py exit code 0, test_model.py exit code 0 — all independent
checks passed.

## 7. Conceptual limits
- Classical KE at 0.03c underestimates the relativistic value by about
  0.068%; adequate at Pre-Phase A, but the approximation must be revisited
  well before 0.1c.
- Travel time assumes instantaneous acceleration to cruise speed;
  acceleration/deceleration phases and relativistic effects are excluded.
- Dust energy is bare classical impact KE; flux, shielding and ablation are
  out of scope.
- A spin gravity of about 4.47 g0 far exceeds 1 g0: the 1000 m / 2 rpm
  habitat is a reference case, not a crewed design point; structural loads and
  Coriolis limits are out of scope.
