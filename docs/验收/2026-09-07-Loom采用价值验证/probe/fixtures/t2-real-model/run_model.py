# ============================================================================
# run_model.py — 统一模型 CLI（有界退出）
# ----------------------------------------------------------------------------
# 用法: python3 run_model.py [--out-dir DIR]
# 动作: 1) 加载冻结基线并核对 SHA-256；2) 运行 §4.1 八类计算；
#       3) 与 acceptance.json reference_checks 做容差比对（RQ-VER-003）；
#       4) 写出 model_output.json 与 ../app/data/baseline_params.json（应用参数，
#          ICD-SOT-002 同源要求）；5) 任一参考值超差则以退出码 1 失败。
# 追溯键: baseline_id=corona-baseline-1.0.0 baseline_version=1.0.0
#         content_digest=cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2
# ============================================================================
"""Run the unified model, check reference tolerances, emit JSON artifacts."""

from __future__ import annotations

import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from corona_model import Baseline
from corona_model import computations, physics

GENERATED_BY = "corona-model 1.0.0 (corona-prephase-a-lab-v3-luna-digital-engineering-builder)"


def reference_check_results(baseline: Baseline, records: list) -> list:
    """Map acceptance.json reference_checks onto computed values and compare."""
    by_id = {r["id"]: r for r in records}
    computed = {
        "travel_years_at_0_03c": by_id["travel_and_light_time"]["outputs"]["travel_time_yr"],
        "kinetic_energy_1mt_at_0_03c_j":
            by_id["kinetic_energy_comparison"]["outputs"]["crewed_1mt"]["kinetic_energy_classical_j"],
        "kinetic_energy_5mt_at_0_03c_j":
            by_id["kinetic_energy_comparison"]["outputs"]["orbital_material_5mt"]["kinetic_energy_classical_j"],
        "gravity_1km_2rpm_m_s2":
            by_id["artificial_gravity"]["outputs"]["artificial_gravity_m_s2"],
        "dust_1mg_0_03c_j": by_id["dust_impact"]["outputs"]["dust_1mg_energy_j"],
    }
    results = []
    for name, spec in baseline.reference_checks.items():
        actual = computed[name]
        expected = float(spec["expected"])
        tol = float(spec["relative_tolerance"])
        rel_dev = abs(actual - expected) / abs(expected) if expected else abs(actual - expected)
        results.append({
            "check": name,
            "expected": expected,
            "computed": actual,
            "relative_deviation": rel_dev,
            "relative_tolerance": tol,
            "pass": rel_dev <= tol,
            "fact_label": "derived_result",
        })
    return results


def build_app_params(baseline: Baseline, records: list) -> dict:
    """应用参数 JSON（ICD-SOT-002/ICD-DLV-003）：应用与模型同源。

    仅含参数与公式版本，不含计算逻辑；应用侧 calc.js 实现同一组公式。
    """
    c = baseline.constants
    return {
        "traceability": baseline.traceability,
        "generated_by": GENERATED_BY,
        "formula_set": "ICD-FML-001..005 (corona_model/physics.py ≡ app/calc.js)",
        "scope": {
            "cruise_speed_c_locked": baseline.cruise_scenarios[0],
            "scenario_lock": True,
            "note": ("单一 0.03c 为本轮唯一批准情景；0.01c/0.05c 为 CCR-001 待确认变更，"
                     "应用仅预留接口不实现"),
        },
        "constants": c,
        "reference_cases": baseline.reference_cases,
        "reference_checks": baseline.reference_checks,
        "routes": baseline.routes,
        "route_reference_mass_kg": computations.ROUTE_REFERENCE_MASS_KG,
        "target_separations_deg": computations.TARGET_SEPARATIONS_DEG,
        "precursor_probe_speed_c": computations.PRECURSOR_PROBE_SPEED_C,
        "tnt_equivalent_j_per_kg": physics.TNT_EQUIVALENT_J_PER_KG,
        "truth_labels": baseline.truth_labels,
        "forbidden_claims": baseline.forbidden_claims,
        "route_evidence_status": {
            "laser_sail_precursor": {
                "evidence_status": "概念级可行方向，已有公开计划参照（Starshot 类）",
                "成立条件": "激光阵列功率/成本、光帆材料与通信链路经缩比验证",
                "终止条件": "单位功率成本或帆面存活率达不到量级要求且无路线路径",
                "主要未知项": ["尘埃通量与帆面存活率 [unknown]", "激光阵列长期维护成本 [unknown]"],
                "fact_label": "assumption",
            },
            "uncrewed_civilization_archive": {
                "evidence_status": "存储介质可分别研究；整体载荷为概念级",
                "成立条件": "存储校验/解码说明/读取设备再制造方案闭合；运输方案另行论证",
                "终止条件": "无可验证的长期读取与复苏路径",
                "主要未知项": ["百年级介质误码率 [unknown]", "样本复苏可行性 [unknown]", "运输方案 [unknown]"],
                "fact_label": "assumption",
            },
            "crewed_interstellar_vehicle": {
                "evidence_status": "远期研究对象；推进/封闭生态/辐射防护/在轨工业/社会治理均未成熟",
                "成立条件": "多项关键技术经独立评审通过且质量/能源预算闭合",
                "终止条件": "任一关键技术缺口在第 12 个月架构比较门不可关闭",
                "主要未知项": ["推进效率 [unknown]", "封闭生态百年运行 [unknown]", "长期冬眠不可用为基线 [verified_fact]"],
                "fact_label": "assumption",
            },
        },
    }


def main() -> int:
    parser = argparse.ArgumentParser(description="日冕统一计算模型（baseline 1.0.0, 0.03c 锁定）")
    parser.add_argument("--out-dir", default=os.path.dirname(os.path.abspath(__file__)),
                        help="模型输出目录（默认: outputs/model/）")
    parser.add_argument("--app-data", default=os.path.join(
        os.path.dirname(os.path.abspath(__file__)), os.pardir, "app", "data", "baseline_params.json"),
        help="应用参数 JSON 输出路径")
    args = parser.parse_args()

    baseline = Baseline()
    trace = baseline.traceability
    print(f"[model] baseline_id={trace['baseline_id']} version={trace['baseline_version']}")
    print(f"[model] content_digest={trace['content_digest']} (sha256 verified against frozen record)")
    print(f"[model] cruise scenarios (locked): {baseline.cruise_scenarios}")

    records = computations.run_all(baseline)
    checks = reference_check_results(baseline, records)

    for r in checks:
        status = "PASS" if r["pass"] else "FAIL"
        print(f"[reference_check] {status} {r['check']}: computed={r['computed']:.10g} "
              f"expected={r['expected']:.10g} rel_dev={r['relative_deviation']:.3e} "
              f"tol={r['relative_tolerance']:.3e}")

    model_output = {
        "traceability": trace,
        "generated_by": GENERATED_BY,
        "scope_statement": ("Level-0 / Pre-Phase A 概念级计算；单一 0.03c 情景；"
                            "动能仅为下限，非完整推进能源预算；三路线可分离"),
        "categories": records,
        "reference_check_results": checks,
        "all_reference_checks_pass": all(r["pass"] for r in checks),
    }
    out_path = os.path.join(args.out_dir, "model_output.json")
    with open(out_path, "w", encoding="utf-8") as fh:
        json.dump(model_output, fh, ensure_ascii=False, indent=2)
    print(f"[model] wrote {out_path}")

    app_params = build_app_params(baseline, records)
    os.makedirs(os.path.dirname(os.path.abspath(args.app_data)), exist_ok=True)
    with open(args.app_data, "w", encoding="utf-8") as fh:
        json.dump(app_params, fh, ensure_ascii=False, indent=2)
    print(f"[model] wrote {args.app_data}")
    # 同步生成 <script> 可加载副本（离线 file:// 场景下 fetch 受 CORS 限制，
    # 应用以 window.CORONA_PARAMS 兜底，保证无云服务、无本地服务器也可运行）。
    js_path = os.path.splitext(args.app_data)[0] + ".js"
    with open(js_path, "w", encoding="utf-8") as fh:
        fh.write("// 由 run_model.py 自动生成；内容与 baseline_params.json 完全一致\n")
        fh.write("window.CORONA_PARAMS = ")
        json.dump(app_params, fh, ensure_ascii=False, indent=2)
        fh.write(";\n")
    print(f"[model] wrote {js_path}")

    if not model_output["all_reference_checks_pass"]:
        print("[model] REFERENCE CHECK FAILURE", file=sys.stderr)
        return 1
    print("[model] all reference checks within tolerance")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
