"""test_model.py - independent verification of results.json.

Checks are derived here from first principles (constants re-entered, nothing
imported from model.py except its YAML parser to read the baseline reference
checks), so an arithmetic error in model.py cannot be masked.
"""

import hashlib
import json
import math
import pathlib
import sys

NUM_KEYS = ['energy_1mt_j', 'energy_5mt_j', 'dust_j', 'travel_years', 'gravity_m_s2']
FROZEN_SHA256 = 'cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2'
FAILURES = []


def record(name, ok, detail):
    print(('PASS' if ok else 'FAIL'), '-', name, '-', detail)
    if not ok:
        FAILURES.append(name)


def close(a, b, rel):
    return abs(a - b) <= rel * max(abs(a), abs(b), 1e-300)


def match_key(check_name):
    n = check_name.lower()
    if '1mt' in n:
        return 'energy_1mt_j'
    if '5mt' in n:
        return 'energy_5mt_j'
    if 'dust' in n:
        return 'dust_j'
    if 'travel_years' in n:
        return 'travel_years'
    if 'gravity' in n:
        return 'gravity_m_s2'
    return None


def main():
    results_path = pathlib.Path('results.json')
    record('results.json exists', results_path.is_file(), str(results_path))
    results = json.loads(results_path.read_text(encoding='utf-8'))

    # 1. schema: required numeric keys are numbers, baseline_sha256 is a string
    for key in NUM_KEYS:
        val = results.get(key)
        record('key %s is numeric' % key,
               isinstance(val, (int, float)) and not isinstance(val, bool),
               repr(val))
    record('baseline_sha256 is str', isinstance(results.get('baseline_sha256'), str),
           repr(results.get('baseline_sha256')))

    # 2. baseline identity: input untouched, digest matches frozen value
    raw = pathlib.Path('baseline_frozen.yaml').read_bytes()
    digest = hashlib.sha256(raw).hexdigest()
    record('baseline file sha256 == frozen digest', digest == FROZEN_SHA256, digest)
    record('results baseline_sha256 == file digest',
           results['baseline_sha256'] == digest, results['baseline_sha256'])

    # 3. first-principles recomputation, independent of model.py
    c = 299792458.0              # m/s, exact SI
    beta = 0.03                  # single approved cruise speed (fraction of c)
    v = beta * c
    year_s = 365.25 * 86400.0    # Julian year in seconds

    e1 = 0.5 * 1.0e9 * v * v             # crewed_1mt: 1 Mt = 1e9 kg
    e5 = 0.5 * 5.0e9 * v * v             # orbital_material_5mt: 5e9 kg
    dust = 0.5 * 1.0e-6 * v * v          # dust_1mg: 1 mg = 1e-6 kg
    ly_m = c * year_s
    years = 4.25 * ly_m / v / year_s     # Proxima 4.25 ly at 0.03c
    omega = 2.0 * (2.0 * math.pi / 60.0) # 2 rpm -> rad/s
    grav = omega * omega * 1000.0        # a = omega^2 r, r = 1000 m

    record('energy_1mt_j first principles',
           close(results['energy_1mt_j'], e1, 1e-12), 'expected %.10e' % e1)
    record('energy_5mt_j first principles',
           close(results['energy_5mt_j'], e5, 1e-12), 'expected %.10e' % e5)
    record('energy_5mt_j == 5 x energy_1mt_j',
           close(results['energy_5mt_j'], 5.0 * results['energy_1mt_j'], 1e-12),
           'ratio %.15f' % (results['energy_5mt_j'] / results['energy_1mt_j']))
    record('dust_j first principles',
           close(results['dust_j'], dust, 1e-12), 'expected %.10e' % dust)
    record('travel_years first principles',
           close(results['travel_years'], years, 1e-12), 'expected %.10f' % years)
    record('travel_years == 4.25 / 0.03',
           close(results['travel_years'], 4.25 / 0.03, 1e-12),
           'expected %.10f' % (4.25 / 0.03))
    record('gravity_m_s2 first principles',
           close(results['gravity_m_s2'], grav, 1e-12), 'expected %.10f' % grav)

    # 4. cross-check against the baseline's own published reference_checks.
    #    Structure (verified by diagnostic): a dict of 5 entries,
    #    name -> {expected: float, relative_tolerance: float}.
    sys.path.insert(0, '.')
    import model  # parser only; expected values come from the baseline file
    cfg = model.parse_simple_yaml(raw.decode('utf-8'))
    checks = cfg.get('reference_checks') or {}
    print('parsed reference_checks entries:', len(checks))
    matched = set()
    for cname, spec in checks.items():
        if not isinstance(spec, dict):
            print('  (non-dict entry, skipped):', cname)
            continue
        target = match_key(str(cname))
        if target is None:
            print('  (unmatched entry, skipped):', cname)
            continue
        exp = spec.get('expected')
        rel_tol = spec.get('relative_tolerance')
        if not isinstance(exp, (int, float)):
            record('reference_check %s exposes an expected value' % cname, False,
                   json.dumps(spec, default=str))
            continue
        actual = float(results[target])
        tol = float(rel_tol) if isinstance(rel_tol, (int, float)) else 1e-9
        ok = close(actual, float(exp), tol)
        record('reference_check %s -> %s' % (cname, target), ok,
               'actual=%.10e expected=%.10e rel_tol=%g' % (actual, float(exp), tol))
        matched.add(target)
    missing = [k for k in NUM_KEYS if k not in matched]
    record('all five quantities covered by reference_checks', not missing,
           'missing=%s' % missing)

    print()
    if FAILURES:
        print('RESULT: %d check(s) FAILED: %s' % (len(FAILURES), FAILURES))
        return 1
    print('RESULT: all independent checks passed')
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
