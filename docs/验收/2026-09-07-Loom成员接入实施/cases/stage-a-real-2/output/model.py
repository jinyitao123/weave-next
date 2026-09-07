"""model.py — Corona Pre-Phase A frozen-baseline validator.

Reads baseline_frozen.yaml (sole input, never modified) and computes, for the
single approved 0.03c cruise scenario:
  * classical kinetic energy of the crewed_1mt and orbital_material_5mt cases
  * classical kinetic (impact) energy of the dust_1mg case
  * cruise travel time to Proxima (years)
  * rotating-habitat centripetal gravity (m/s^2)
Writes results.json with numeric results and the SHA-256 of the baseline file.
Standard library only.
"""

import hashlib
import json
import math
import pathlib

BASELINE = pathlib.Path("baseline_frozen.yaml")
RESULTS = pathlib.Path("results.json")
SECONDS_PER_YEAR = 365.25 * 24.0 * 3600.0


def _strip_comment(line):
    """Remove a trailing # comment that is not inside quotes."""
    out = []
    in_s = in_d = False
    for i, ch in enumerate(line):
        if ch == "'" and not in_d:
            in_s = not in_s
        elif ch == chr(34) and not in_s:
            in_d = not in_d
        elif ch == "#" and not in_s and not in_d:
            if i == 0 or line[i - 1] in (" ", chr(9)):
                break
        out.append(ch)
    return "".join(out).rstrip()


def _scalar(text):
    """Parse a scalar from the simple YAML subset used by the baseline."""
    text = text.strip()
    if text == "":
        return None
    if text.startswith(chr(34)) and text.endswith(chr(34)):
        return json.loads(text)  # handles YAML/JSON-style escapes in doubles
    if text.startswith("'") and text.endswith("'"):
        return text[1:-1]
    if text.startswith("[") and text.endswith("]"):
        inner = text[1:-1].strip()
        return [] if not inner else [_scalar(p) for p in inner.split(",")]
    low = text.lower()
    if low in ("true", "false"):
        return low == "true"
    if low in ("null", "~"):
        return None
    try:
        return int(text)
    except ValueError:
        pass
    try:
        return float(text)
    except ValueError:
        pass
    return text


def parse_simple_yaml(text):
    """Minimal parser for the baseline subset: nested maps, lists of maps,
    scalars, quoted strings, #-comments; folded/literal block scalars are
    consumed and ignored (the model never needs their prose)."""
    raw_lines = text.splitlines()
    lines = []
    for raw in raw_lines:
        if not raw.strip() or raw.lstrip().startswith("#"):
            continue
        indent = len(raw) - len(raw.lstrip())
        lines.append((indent, _strip_comment(raw.strip())))
    lines = [(i, s) for i, s in lines if s]

    def parse_block(idx, indent):
        container = None
        while idx < len(lines):
            ind, content = lines[idx]
            if ind < indent:
                break
            if ind > indent:
                idx += 1  # stray deeper line (e.g. folded text); skip
                continue
            if content.startswith("- ") or content == "-":
                if container is None:
                    container = []
                item_text = content[1:].strip()
                if not item_text:
                    item, idx = parse_block(idx + 1, indent + 2)
                    container.append(item)
                elif ":" in item_text and not item_text.startswith(("'", chr(34))):
                    key, _, val = item_text.partition(":")
                    item = {key.strip(): _scalar(val)}
                    idx += 1
                    sub, idx = parse_block(idx, indent + 2)
                    if isinstance(sub, dict):
                        item.update(sub)
                    container.append(item)
                else:
                    container.append(_scalar(item_text))
                    idx += 1
            else:
                if container is None:
                    container = {}
                if isinstance(container, list):
                    break
                key, _, val = content.partition(":")
                key = key.strip()
                val = val.strip()
                if val in ("-", ">", ">-", "|", "|-"):
                    # folded/literal block scalar: consume deeper lines
                    idx += 1
                    while idx < len(lines) and lines[idx][0] > indent:
                        idx += 1
                    container[key] = None
                elif val:
                    container[key] = _scalar(val)
                    idx += 1
                else:
                    sub, idx = parse_block(idx + 1, indent + 2)
                    container[key] = sub
        return container, idx

    root, _ = parse_block(0, 0)
    return root


def compute(cfg, digest):
    """Pure computation from parsed baseline config; returns results dict."""
    consts = cfg["constants"]
    c = float(consts["speed_of_light_m_s"])
    dist_ly = float(consts["proxima_distance_ly"])

    cases = {case["id"]: case for case in cfg["reference_cases"]}
    speed_c = float(cfg["scenarios"]["cruise_speed_c"][0])
    v = speed_c * c

    crewed = cases["crewed_1mt"]
    material = cases["orbital_material_5mt"]
    dust = cases["dust_1mg"]
    habitat = cases["rotating_habitat"]

    assert float(crewed["cruise_speed_c"]) == speed_c
    assert float(material["cruise_speed_c"]) == speed_c
    assert float(dust["relative_speed_c"]) == speed_c

    energy_1mt = 0.5 * float(crewed["mass_kg"]) * v ** 2
    energy_5mt = 0.5 * float(material["mass_kg"]) * v ** 2
    dust_e = 0.5 * float(dust["mass_kg"]) * v ** 2

    ly_m = c * SECONDS_PER_YEAR  # one light-year in metres
    travel_years = dist_ly * ly_m / v / SECONDS_PER_YEAR  # == dist_ly / speed_c

    omega = float(habitat["rotation_rpm"]) * 2.0 * math.pi / 60.0
    gravity = omega ** 2 * float(habitat["radius_m"])

    return {
        "energy_1mt_j": energy_1mt,
        "energy_5mt_j": energy_5mt,
        "dust_j": dust_e,
        "travel_years": travel_years,
        "gravity_m_s2": gravity,
        "baseline_sha256": digest,
    }


def main():
    raw = BASELINE.read_bytes()
    digest = hashlib.sha256(raw).hexdigest()
    cfg = parse_simple_yaml(raw.decode("utf-8"))
    results = compute(cfg, digest)
    RESULTS.write_text(json.dumps(results, indent=2) + chr(10), encoding="utf-8")

    print("baseline_id:", cfg.get("baseline_id"))
    print("scenario cruise_speed_c:", float(cfg["scenarios"]["cruise_speed_c"][0]))
    for k, val in results.items():
        print(k + ":", val)


if __name__ == "__main__":
    main()
