#!/usr/bin/env python3
"""make_params.py — 从冻结基线 baseline_frozen.yaml 生成机器可读参数副本 params.json。

追溯键: baseline_id = corona-baseline-1.0.0 · baseline_version = 1.0.0
        content_digest = cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2

设计说明 [verified_fact]:
- 系统 Python 无 PyYAML（上游 luna-mission-lead 已如实记录），故本脚本内置一个针对
  冻结基线文件结构的 YAML 子集解析器（标量 / 行内列表 / 嵌套映射 / 块列表 / 折叠标量）。
- 生成时将 baseline_frozen.yaml 的 SHA-256 摘要嵌入 params.json，
  模型测试据此做双向追溯校验（ICD-SOT-003 / DEC-004）。
- 命令有界：单次读取、单次写出，自行退出。
"""
import hashlib
import json
import os

BASELINE_FILE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "baseline_frozen.yaml")
OUT_FILE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "params.json")


def _strip_comment(line: str) -> str:
    in_s = in_d = False
    for idx, ch in enumerate(line):
        if ch == "'" and not in_d:
            in_s = not in_s
        elif ch == '"' and not in_s:
            in_d = not in_d
        elif ch == "#" and not in_s and not in_d:
            if idx == 0 or line[idx - 1] in " \t":
                return line[:idx]
    return line


def _scalar(text: str):
    text = text.strip()
    if text == "":
        return None
    if (text.startswith('"') and text.endswith('"')) or (text.startswith("'") and text.endswith("'")):
        return text[1:-1]
    if text.startswith("[") and text.endswith("]"):
        inner = text[1:-1].strip()
        if not inner:
            return []
        return [_scalar(part) for part in inner.split(",")]
    low = text.lower()
    if low in ("true", "false"):
        return low == "true"
    try:
        return int(text)
    except ValueError:
        pass
    try:
        return float(text)
    except ValueError:
        pass
    return text


def _tokenize(path: str):
    """把 YAML 行序列变为 (indent, content) 序列，折叠标量合并为单行。"""
    raw = []
    with open(path, "r", encoding="utf-8") as fh:
        for line in fh:
            line = _strip_comment(line.rstrip("\n"))
            if line.strip() == "":
                continue
            indent = len(line) - len(line.lstrip(" "))
            raw.append((indent, line.strip()))
    out = []
    i = 0
    while i < len(raw):
        indent, content = raw[i]
        if content.endswith(": >-") or content.endswith(": >") or content.endswith(": |-") or content.endswith(": |"):
            key = content.split(":", 1)[0]
            parts = []
            j = i + 1
            while j < len(raw) and raw[j][0] > indent:
                parts.append(raw[j][1])
                j += 1
            out.append((indent, "%s: %s" % (key, " ".join(parts))))
            i = j
        else:
            out.append((indent, content))
            i += 1
    return out


def _parse_block(lines, i, indent):
    """解析 indent 层级的一个块，返回 (对象, 下一行下标)。"""
    if i >= len(lines):
        return None, i
    is_list = lines[i][1].startswith("- ")
    if is_list:
        result = []
        while i < len(lines) and lines[i][0] == indent and lines[i][1].startswith("- "):
            content = lines[i][1][2:].strip()
            if ":" in content:
                item = {}
                key, _, val = content.partition(":")
                val = val.strip()
                i += 1
                if val:
                    item[key.strip()] = _scalar(val)
                else:
                    child, i = _parse_block(lines, i, indent + 2)
                    item[key.strip()] = child
                while i < len(lines) and lines[i][0] > indent:
                    k2, _, v2 = lines[i][1].partition(":")
                    v2 = v2.strip()
                    i += 1
                    if v2:
                        item[k2.strip()] = _scalar(v2)
                    else:
                        child, i = _parse_block(lines, i, lines[i - 1][0] + 2)
                        item[k2.strip()] = child
                result.append(item)
            else:
                result.append(_scalar(content))
                i += 1
        return result, i
    result = {}
    while i < len(lines) and lines[i][0] == indent and not lines[i][1].startswith("- "):
        key, _, val = lines[i][1].partition(":")
        key = key.strip()
        val = val.strip()
        i += 1
        if val:
            result[key] = _scalar(val)
        else:
            child, i = _parse_block(lines, i, indent + 2)
            result[key] = child
    return result, i


def load_baseline(path: str = BASELINE_FILE) -> dict:
    lines = _tokenize(path)
    obj, _ = _parse_block(lines, 0, lines[0][0] if lines else 0)
    return obj


def main() -> int:
    with open(BASELINE_FILE, "rb") as fh:
        digest = hashlib.sha256(fh.read()).hexdigest()
    baseline = load_baseline(BASELINE_FILE)

    params = {
        "schema_version": 1,
        "traceability": {
            "baseline_id": baseline["baseline_id"],
            "baseline_version": baseline["baseline_version"],
            "content_digest": digest,
            "derived_from": "baseline_frozen.yaml",
            "generator": "make_params.py",
            "fact_label": "verified_fact",
        },
        "scenarios": {
            "cruise_speed_c": baseline["scenarios"]["cruise_speed_c"],
            "locked": baseline["scenarios"]["locked"],
            "fact_label": baseline["scenarios"]["fact_label"],
        },
        "constants": baseline["constants"],
        "reference_cases": baseline["reference_cases"],
        "reference_checks": baseline["reference_checks"],
        "routes": baseline["routes"],
        "truth_labels": baseline["truth_labels"],
        "forbidden_claims": baseline["forbidden_claims"],
        "pending_changes": baseline["pending_changes"],
        "precursor_probe": {
            "cruise_speed_c": 0.2,
            "fact_label": "verified_fact",
            "source": "纪要 §二.6；ICD-RTE-003（路线内部工况，非巡航情景扩展）",
        },
        "target_geometry_deg": {
            "proxima_barnard": 78.0,
            "proxima_tau_ceti": 101.0,
            "barnard_tau_ceti": 117.0,
            "fact_label": "verified_fact",
            "source": "纪要 §二.5（公开天球坐标计算结果，如纪要所载）",
        },
        "tnt_equivalent_j_per_kg": {
            "value": 4.184e6,
            "fact_label": "assumption",
            "source": "常用换算约定（ICD-FML-004 注）",
        },
    }

    with open(OUT_FILE, "w", encoding="utf-8") as fh:
        json.dump(params, fh, ensure_ascii=False, indent=2, sort_keys=False)
        fh.write("\n")

    print("baseline_id      = %s" % params["traceability"]["baseline_id"])
    print("baseline_version = %s" % params["traceability"]["baseline_version"])
    print("content_digest   = %s" % digest)
    print("scenarios        = %s (locked=%s)" % (params["scenarios"]["cruise_speed_c"], params["scenarios"]["locked"]))
    print("written          = %s" % OUT_FILE)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
