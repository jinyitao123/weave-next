#!/usr/bin/env python3
"""
test_model.py - 日冕计划统一计算内核测试套件 (unittest)

运行方式（从 outputs/model 目录下）:
    python3 -m unittest tests.test_model -v
    python3 -m unittest discover -q

本套件覆盖:
  - 冻结常量与基线字段
  - 三情景 ID / 速度轴贯穿
  - acceptance.json 五项 0.03c 参考锚点复算（reference_calculations_within_tolerance）
  - 各情景派生量（航时、动能下限、TNT 当量、人工重力、尘埃撞击能、相对论修正、束动力比）
  - 参数一致性（params.json == baseline.yaml 关键字段）
  - 事实纪律（forbidden_claims 未使用、真值标签存在）
"""

import os
import sys
import unittest

_HERE = os.path.dirname(os.path.abspath(__file__))
_MODEL_DIR = os.path.dirname(_HERE)
if _MODEL_DIR not in sys.path:
    sys.path.insert(0, _MODEL_DIR)

import corona_model as cm  # noqa: E402


class TestConstants(unittest.TestCase):
    def test_speed_of_light(self):
        self.assertAlmostEqual(cm.C_LIGHT, 299792458.0, places=0)

    def test_standard_gravity(self):
        self.assertAlmostEqual(cm.G0, 9.80665, places=5)

    def test_proxima_distance(self):
        self.assertEqual(cm.DIST_PROXIMA_LY, 4.25)

    def test_baseline_id(self):
        self.assertEqual(cm.PARAMS["baseline_id"], "corona-prephase-a-v1")
        self.assertEqual(cm.PARAMS["baseline_version"], "1.0.0")
        self.assertEqual(cm.PARAMS["status"], "frozen_for_validation")

    def test_forbidden_claims_present(self):
        for claim in ("construction_ready", "manufacturing_ready",
                      "flight_certified", "whole_program_cost_committed"):
            self.assertIn(claim, cm.FORBIDDEN_CLAIMS)

    def test_truth_labels_present(self):
        for lbl in ("verified_fact", "derived_result", "assumption", "unknown"):
            self.assertIn(lbl, cm.TRUTH_LABELS)


class TestScenarios(unittest.TestCase):
    def test_three_speed_scenarios(self):
        self.assertEqual(cm.CRUISE_SPEEDS_C, [0.01, 0.03, 0.05])
        self.assertEqual(cm.SCENARIO_IDS, ["S-0.01c", "S-0.03c", "S-0.05c"])

    def test_scenario_id_roundtrip(self):
        self.assertEqual(cm.scenario_id_for_c(0.01), "S-0.01c")
        self.assertEqual(cm.scenario_id_for_c(0.03), "S-0.03c")
        self.assertEqual(cm.scenario_id_for_c(0.05), "S-0.05c")


class TestReferenceAnchors(unittest.TestCase):
    """acceptance.json 五项 0.03c 锚点复算（相对容差内）。"""

    def test_reference_check_all_ok(self):
        result = cm.reference_check()
        self.assertTrue(result["all_ok"])
        self.assertEqual(len(result["rows"]), 5)
        for row in result["rows"]:
            self.assertTrue(row["ok"], f"{row['name']} rel_err={row['rel_err']}")

    def test_travel_years_0_03c(self):
        self.assertAlmostEqual(cm.time_alpha_yrs(0.03), 141.6666667, places=4)

    def test_ke_1mt_0_03c(self):
        self.assertAlmostEqual(cm.ke_1mt_j(0.03), 4.0443983e22, delta=4.0443983e22 * 0.01)

    def test_ke_5mt_0_03c(self):
        self.assertAlmostEqual(cm.ke_5mt_j(0.03), 2.0221991e23, delta=2.0221991e23 * 0.01)

    def test_gravity_1km_2rpm(self):
        self.assertAlmostEqual(cm.gravity_rotating_habitat(1000.0, 2.0), 43.8649, delta=0.438649)

    def test_dust_1mg_0_03c(self):
        self.assertAlmostEqual(cm.ke_dust_1mg_j(0.03), 40443983.0, delta=40443983.0 * 0.01)


class TestScenarioDerivables(unittest.TestCase):
    def test_time_alpha(self):
        self.assertAlmostEqual(cm.time_alpha_yrs(0.01), 425.0, places=3)
        self.assertAlmostEqual(cm.time_alpha_yrs(0.05), 85.0, places=3)

    def test_ke_1mt_scaling(self):
        # 0.05c 应为 0.01c 的 25 倍（v^2 缩放）
        ratio = cm.ke_1mt_j(0.05) / cm.ke_1mt_j(0.01)
        self.assertAlmostEqual(ratio, 25.0, places=4)

    def test_ke_5mt_is_five_times_1mt(self):
        ratio = cm.ke_5mt_j(0.03) / cm.ke_1mt_j(0.03)
        self.assertLess(abs(ratio - 5.0), 1e-9)

    def test_tnt_tonnes(self):
        # 0.01c 1mt -> ~1.074e12 吨
        self.assertAlmostEqual(cm.tnt_tonnes(cm.ke_1mt_j(0.01)), 1.0740e12, delta=1.0e10)

    def test_dust_tnt_kg(self):
        # 0.01c 1mg -> ~1.07 kg TNT
        self.assertAlmostEqual(cm.tnt_tonnes(cm.ke_dust_1mg_j(0.01)) * 1000.0, 1.074, places=1)

    def test_relativistic_correction_small(self):
        # 0.05c 相对论修正 <0.2%
        self.assertLess(cm.relativistic_ke_1mt_j(0.05) / cm.ke_1mt_j(0.05) - 1.0, 0.002)

    def test_p_rel_scaling(self):
        # P_REL = (0.2/c)^2
        self.assertAlmostEqual(cm.p_rel(0.01), 400.0, places=2)
        self.assertAlmostEqual(cm.p_rel(0.03), 44.4444, places=2)
        self.assertAlmostEqual(cm.p_rel(0.05), 16.0, places=2)

    def test_beam_power_gw(self):
        # 0.01c -> 0.25 GW, 0.05c -> 6.25 GW
        self.assertAlmostEqual(cm.beam_power_gw(0.01), 0.25, places=2)
        self.assertAlmostEqual(cm.beam_power_gw(0.05), 6.25, places=2)

    def test_time_glens_0_03c(self):
        # 到 550 AU 透镜区约 106 天
        self.assertAlmostEqual(cm.time_glens_yrs(0.03) * 365.25, 105.9, delta=1.5)

    def test_time_precursor_0_03c(self):
        # 到 1000 AU 前驱约 6.3 月
        self.assertAlmostEqual(cm.time_precursor_yrs(0.03) * 12.0, 6.33, delta=0.3)

    def test_compute_scenario_keys(self):
        res = cm.compute_scenario(0.03)
        for key in ("scenario_id", "cruise_speed_c", "v_kmps", "time_alpha_yrs",
                    "ke_1mt_j", "ke_5mt_j", "ke_dust_1mg_j", "p_rel"):
            self.assertIn(key, res)
        self.assertEqual(res["scenario_id"], "S-0.03c")


class TestParamConsistency(unittest.TestCase):
    def test_params_vs_baseline_yaml(self):
        """params.json 与 baseline.yaml 关键字段一致（gate 10 的机器比对项）。"""
        import json
        import mini_yaml

        baseline_path = os.path.join(_MODEL_DIR, "baseline.yaml")
        params_path = os.path.join(_MODEL_DIR, "params.json")

        with open(baseline_path, "r", encoding="utf-8") as fh:
            baseline = mini_yaml.safe_load(fh.read())
        with open(params_path, "r", encoding="utf-8") as fh:
            params = json.load(fh)

        self.assertEqual(params["baseline_id"], baseline["baseline_id"])
        self.assertEqual(params["baseline_version"], baseline["baseline_version"])
        self.assertEqual(params["scenarios"]["cruise_speed_c"],
                         baseline["scenarios"]["cruise_speed_c"])
        self.assertEqual(params["scenarios"]["scenario_ids"],
                         baseline["scenarios"]["scenario_ids"])

        for key in ("speed_of_light_m_s", "standard_gravity_m_s2",
                    "proxima_distance_ly", "ly_m", "sec_per_year",
                    "tnt_equivalent_J_per_kg"):
            self.assertEqual(params["constants"][key], baseline["constants"][key])

        for case in baseline["reference_cases"]:
            match = [p for p in params["reference_cases"] if p["id"] == case["id"]]
            self.assertEqual(len(match), 1)
            m = match[0]
            for key, value in case.items():
                self.assertIn(key, m)
                self.assertEqual(m[key], value)


if __name__ == "__main__":
    unittest.main(verbosity=2)
