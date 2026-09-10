#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
模型自动测试运行器 (有界, 自行结束; 无网络, 无前台服务器)
用法: 从 outputs/model 目录执行 `python3 run_tests.py`
追溯键: baseline_id=corona-baseline-1.0.0 · baseline_version=1.0.0
        content_digest=cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2
覆盖: 任务书 §4.1 八类计算 + acceptance.json 五项参考值容差 + 情景锁定 + 事实标签
"""
import json
import os
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(HERE, "output")

failures = []
checks = []


def check(name, ok, detail=""):
    checks.append({"check": name, "pass": bool(ok), "detail": detail})
    print(("PASS" if ok else "FAIL") + f"  {name}  {detail}")
    if not ok:
        failures.append(name)


def rel_close(actual, expected, tol):
    return abs(actual - expected) <= tol * abs(expected)


def main():
    os.makedirs(OUT, exist_ok=True)

    # --- 步骤 1: 由冻结基线派生参数 (ruby YAML, 含 SHA-256 摘要核验) ---
    r = subprocess.run(["ruby", os.path.join(HERE, "derive_params.rb")],
                       capture_output=True, text=True, timeout=60)
    print("[derive] " + (r.stdout.strip() or r.stderr.strip()))
    check("baseline_digest_verified_and_params_derived", r.returncode == 0,
          (r.stdout or r.stderr).strip())

    sys.path.insert(0, HERE)
    import corona_model

    params = corona_model.load_parameters()

    # --- 步骤 2: 情景锁定检查 (RQ-SCP-001) ---
    speeds = params["scenarios"]["cruise_speed_c"]
    check("scenario_locked_single_0.03c", speeds == [0.03], f"cruise_speed_c={speeds}")

    # --- 步骤 3: 运行统一计算模型 ---
    results = corona_model.compute_all(params)
    rv = results["reference_values"]

    # --- 步骤 4: acceptance.json 五项参考值容差比对 ---
    rc = params["reference_checks"]
    pairs = [
        ("travel_time_yr", rv["travel_time_yr"],
         rc["travel_years_at_0_03c"]["expected"], rc["travel_years_at_0_03c"]["relative_tolerance"]),
        ("kinetic_energy_1mt_j", rv["kinetic_energy_1mt_j"],
         rc["kinetic_energy_1mt_at_0_03c_j"]["expected"], rc["kinetic_energy_1mt_at_0_03c_j"]["relative_tolerance"]),
        ("kinetic_energy_5mt_j", rv["kinetic_energy_5mt_j"],
         rc["kinetic_energy_5mt_at_0_03c_j"]["expected"], rc["kinetic_energy_5mt_at_0_03c_j"]["relative_tolerance"]),
        ("artificial_gravity_m_s2", rv["artificial_gravity_m_s2"],
         rc["gravity_1km_2rpm_m_s2"]["expected"], rc["gravity_1km_2rpm_m_s2"]["relative_tolerance"]),
        ("dust_impact_energy_j", rv["dust_impact_energy_j"],
         rc["dust_1mg_0_03c_j"]["expected"], rc["dust_1mg_0_03c_j"]["relative_tolerance"]),
    ]
    for name, actual, expected, tol in pairs:
        expected = float(expected)  # YAML 1.1 可能将无符号指数解析为字符串, 统一强转
        tol = float(tol)
        ok = rel_close(actual, expected, tol)
        check(f"reference_tolerance:{name}", ok,
              f"actual={actual!r} expected={expected!r} rel_tol={tol}")

    # --- 步骤 5: 八类计算结果存在性与合理性 ---
    res = results["results"]
    check("calc1_travel_time_and_light_time",
          abs(res["travel_time"]["light_travel_time_yr"]["value"] - 4.25) < 1e-9,
          f"light_time={res['travel_time']['light_travel_time_yr']['value']} yr")

    ke1_nr = res["kinetic_energy"]["crewed_1mt"]["kinetic_energy_nonrel_j"]["value"]
    ke1_rel = res["kinetic_energy"]["crewed_1mt"]["kinetic_energy_rel_j"]["value"]
    check("calc2_relativistic_comparison_separate",
          ke1_rel > ke1_nr and (ke1_rel - ke1_nr) / ke1_nr < 0.01,
          f"nonrel={ke1_nr:.6e} rel={ke1_rel:.6e} (分列不混用)")

    eta_map = res["accel_decel_efficiency"]["crewed_1mt"]["min_input_energy_by_efficiency_j"]
    check("calc3_accel_decel_efficiency_mass_scenarios",
          eta_map["0.1"]["value"] > eta_map["0.3"]["value"] > eta_map["0.5"]["value"] > ke1_nr,
          "η=0.1/0.3/0.5 最小输入能量均大于动能下限且随η递减")

    g = res["artificial_gravity"]
    check("calc4_artificial_gravity",
          rel_close(g["reference_case_m_s2"]["value"], 43.8649, 0.01)
          and abs(g["rpm_for_1g_at_1km"]["value"] - 0.9491) < 0.01,
          f"a={g['reference_case_m_s2']['value']:.4f} m/s^2, rpm@1g={g['rpm_for_1g_at_1km']['value']:.4f}")

    d = res["dust_impact"]
    check("calc5_dust_impact",
          rel_close(d["dust_1mg_tnt_kg"]["value"], 9.6667, 0.02)
          and abs(d["dust_10mg_energy_j"]["value"] - 10 * d["dust_1mg_energy_j"]["value"]) < 1,
          f"1mg={d['dust_1mg_energy_j']['value']:.6e} J ≈ {d['dust_1mg_tnt_kg']['value']:.2f} kg TNT")

    tg = res["target_geometry"]["turn_limit_deg"]
    check("calc6_turn_limits",
          abs(tg["delta_v_10pct"]["value"] - 5.7296) < 0.01
          and abs(tg["delta_v_15pct"]["value"] - 8.5944) < 0.01,
          f"10%→{tg['delta_v_10pct']['value']:.4f}°, 15%→{tg['delta_v_15pct']['value']:.4f}°")

    pp = res["precursor_probe"]
    check("calc7_precursor_return_time",
          abs(pp["earliest_data_at_earth_yr"]["value"] - 25.5) < 0.01,
          f"flight={pp['flight_time_yr_at_0_2c']['value']} + return={pp['data_return_time_yr']['value']}"
          f" = {pp['earliest_data_at_earth_yr']['value']} yr")

    sens = res["route_sensitivity"]
    check("calc8_route_sensitivity",
          set(sens.keys()) == {"laser_sail_precursor", "uncrewed_civilization_archive",
                               "crewed_interstellar_vehicle"}
          and sens["crewed_interstellar_vehicle"]["sensitivity_at_0_03c"]["value"] is not None
          and sens["laser_sail_precursor"]["sensitivity_at_0_03c"]["fact_label"] == "unknown",
          "三路线分别给出; 载人路线含质量敏感性, 另两条保留 unknown")

    # --- 步骤 6: 事实标签纪律: 所有带 value 的结果项必须携带合法标签 ---
    bad_labels = []

    def walk(node, path=""):
        if isinstance(node, dict):
            if "value" in node:
                lab = node.get("fact_label")
                if lab not in ("verified_fact", "derived_result", "assumption", "unknown"):
                    bad_labels.append(path or "<root>")
            for k, v in node.items():
                if k != "traceability":
                    walk(v, f"{path}.{k}" if path else k)
        elif isinstance(node, list):
            for i, v in enumerate(node):
                walk(v, f"{path}[{i}]")

    walk(results["results"])
    check("truth_labels_present_on_all_results", not bad_labels,
          f"mislabeled={bad_labels if bad_labels else 'none'}")

    # --- 步骤 7: 禁用声明扫描 (模型结果文本) ---
    forbidden = ["construction_ready", "manufacturing_ready",
                 "flight_certified", "whole_program_cost_committed"]
    blob = json.dumps(results, ensure_ascii=False)
    found = [w for w in forbidden if w in blob]
    check("no_forbidden_claims_in_model_output", not found,
          f"found={found if found else 'none'}")

    # --- 步骤 8: 落盘输出 ---
    with open(os.path.join(OUT, "reference_values.json"), "w", encoding="utf-8") as f:
        json.dump({"traceability": corona_model.TRACE_KEYS,
                   "reference_values": rv}, f, ensure_ascii=False, indent=2)
    with open(os.path.join(OUT, "model_results.json"), "w", encoding="utf-8") as f:
        json.dump(results, f, ensure_ascii=False, indent=2)
    with open(os.path.join(OUT, "route_sensitivity.json"), "w", encoding="utf-8") as f:
        json.dump({"traceability": corona_model.TRACE_KEYS,
                   "route_sensitivity": sens}, f, ensure_ascii=False, indent=2)

    summary = {
        "traceability": corona_model.TRACE_KEYS,
        "total_checks": len(checks),
        "passed": sum(1 for c in checks if c["pass"]),
        "failed": len(failures),
        "verdict": "PASS" if not failures else "FAIL",
        "checks": checks,
    }
    with open(os.path.join(OUT, "test_results.json"), "w", encoding="utf-8") as f:
        json.dump(summary, f, ensure_ascii=False, indent=2)

    print(f"\n[summary] {summary['passed']}/{summary['total_checks']} checks passed "
          f"-> {summary['verdict']}")
    print("[outputs] output/reference_values.json, output/model_results.json, "
          "output/route_sensitivity.json, output/test_results.json")
    return 0 if not failures else 1


if __name__ == "__main__":
    sys.exit(main())
