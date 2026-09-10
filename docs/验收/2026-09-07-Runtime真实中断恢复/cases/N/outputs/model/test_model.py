"""日冕计划 · 模型测试 (test_model.py)

从 outputs/model 目录执行:  python3 test_model.py
有界退出（纯本地计算，无网络/无服务），退出码 0 = 全部 PASS。

覆盖:
  1. 追溯链校验（baseline_frozen.yaml 实际 SHA-256 == 冻结摘要 == JSON 副本内嵌值）
  2. 情景锁定（仅 0.03c，locked=true）
  3. acceptance.json 五项 reference_checks 容差比对
  4. §4.1 八类计算的存在性与自洽性（含相对论对照、加减速边界、转向上限、
     先锋探测器时间线、三路线敏感性与可分离性注记）
"""

import json
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import corona_model as cm  # noqa: E402


class TestTraceability(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.params = cm.load_baseline()

    def test_yaml_digest_matches_frozen_record(self):
        self.assertEqual(cm.sha256_of(cm.BASELINE_YAML), cm.EXPECTED_DIGEST)

    def test_traceability_triple(self):
        t = self.params["_traceability"]
        self.assertEqual(t["baseline_id"], "corona-baseline-1.0.0")
        self.assertEqual(t["baseline_version"], "1.0.0")
        self.assertEqual(t["content_digest"], cm.EXPECTED_DIGEST)

    def test_scenario_lock_single_0_03c(self):
        self.assertEqual(cm.locked_cruise_speed_c(self.params), 0.03)
        self.assertEqual(self.params["scenarios"]["cruise_speed_c"], [0.03])

    def test_pending_ccr001_not_in_baseline(self):
        ccr = [c for c in self.params["pending_changes"] if c["id"] == "CCR-001"]
        self.assertEqual(len(ccr), 1)
        self.assertEqual(ccr[0]["in_baseline"], False)
        self.assertEqual(ccr[0]["status"], "pending_platform_confirmation")


class TestReferenceChecks(unittest.TestCase):
    """acceptance.json reference_checks 容差比对（期望值来自冻结基线）。"""

    @classmethod
    def setUpClass(cls):
        cls.params = cm.load_baseline()
        cls.results = cm.run_all(cls.params)
        cls.checks = cls.params["reference_checks"]

    def _within(self, value, check_key):
        chk = self.checks[check_key]
        exp, tol = chk["expected"], chk["relative_tolerance"]
        self.assertLessEqual(
            abs(value - exp) / abs(exp), tol,
            msg="%s: got %.10g expected %.10g tol %.4g" % (check_key, value, exp, tol),
        )

    def test_travel_time(self):
        self._within(self.results["travel_time"]["travel_time_yr"], "travel_years_at_0_03c")

    def test_kinetic_energy_1mt(self):
        self._within(self.results["kinetic_energy"]["crewed_1mt_classical_j"],
                     "kinetic_energy_1mt_at_0_03c_j")

    def test_kinetic_energy_5mt(self):
        self._within(self.results["kinetic_energy"]["orbital_material_5mt_classical_j"],
                     "kinetic_energy_5mt_at_0_03c_j")

    def test_artificial_gravity(self):
        self._within(self.results["artificial_gravity"]["centripetal_m_s2"],
                     "gravity_1km_2rpm_m_s2")

    def test_dust_impact(self):
        self._within(self.results["dust_impact"]["energy_j"], "dust_1mg_0_03c_j")


class TestComputationCoverage(unittest.TestCase):
    """任务书 §4.1 八类计算的存在性与自洽性。"""

    @classmethod
    def setUpClass(cls):
        cls.params = cm.load_baseline()
        cls.r = cm.run_all(cls.params)

    def test_eight_sections_present(self):
        for key in ("travel_time", "kinetic_energy", "efficiency_and_delta_v",
                    "artificial_gravity", "dust_impact", "turn_limit",
                    "precursor_probe", "route_sensitivity"):
            self.assertIn(key, self.r, msg="missing section %s" % key)

    def test_relativistic_exceeds_classical_and_is_separate(self):
        ke = self.r["kinetic_energy"]
        self.assertGreater(ke["crewed_1mt_relativistic_j"], ke["crewed_1mt_classical_j"])
        # 比值 = 2(γ−1)/β²，β=0.03 → ≈1.0006755（非 1+β²/4，勿混淆）
        self.assertAlmostEqual(ke["relativistic_over_classical_ratio_1mt"], 1.0006755, places=5)

    def test_light_travel_time(self):
        self.assertAlmostEqual(self.r["travel_time"]["light_travel_time_yr"], 4.25)

    def test_accel_decel_boundary(self):
        b = self.r["efficiency_and_delta_v"]["accel_decel_boundary"]
        self.assertAlmostEqual(b["flyby_min_delta_v_c"], 0.03)
        self.assertAlmostEqual(b["rendezvous_min_delta_v_c"], 0.06)

    def test_efficiency_lower_bound_monotonic(self):
        e = self.r["efficiency_and_delta_v"]["crewed_1mt_energy_input_lower_bound_j"]
        self.assertGreater(e["0.1"], e["0.3"])
        self.assertGreater(e["0.3"], e["0.5"])

    def test_1g_rpm_at_1km(self):
        self.assertAlmostEqual(self.r["artificial_gravity"]["rpm_for_1g_at_1km"], 0.949, places=2)

    def test_turn_limit_range(self):
        t = self.r["turn_limit"]
        self.assertAlmostEqual(t["dv_10pct_turn_deg"], 5.73, places=2)
        self.assertAlmostEqual(t["dv_15pct_turn_deg"], 8.59, places=2)
        self.assertGreater(t["target_separations_deg"]["proxima_barnard"],
                           t["dv_15pct_turn_deg"])

    def test_precursor_timeline(self):
        p = self.r["precursor_probe"]
        self.assertAlmostEqual(p["flight_yr"], 21.25)
        self.assertAlmostEqual(p["earliest_roundtrip_yr"], 25.5)

    def test_route_separability_note_and_no_cross_route_extrapolation(self):
        rs = self.r["route_sensitivity"]
        self.assertEqual(set(rs), {"laser_sail_precursor",
                                   "uncrewed_civilization_archive",
                                   "crewed_interstellar_vehicle"})
        for rid, entry in rs.items():
            self.assertIn("separability_note", entry)
            self.assertIn("mass_fact_label", entry)
        self.assertEqual(rs["crewed_interstellar_vehicle"]["mass_fact_label"], "verified_fact")
        self.assertEqual(rs["laser_sail_precursor"]["mass_fact_label"], "assumption")

    def test_dust_tnt_equivalent_order(self):
        self.assertAlmostEqual(self.r["dust_impact"]["tnt_equivalent_kg"], 9.67, places=1)


class TestResultsFileConsistency(unittest.TestCase):
    """run_model.py 产物 results.json 与内存计算一致（若文件存在）。"""

    def test_results_json_matches_recompute_if_present(self):
        path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "results.json")
        if not os.path.exists(path):
            self.skipTest("results.json not generated yet (run: python3 corona_model.py)")
        with open(path, encoding="utf-8") as f:
            on_disk = json.load(f)
        fresh = cm.run_all(cm.load_baseline())
        self.assertEqual(on_disk, fresh)


if __name__ == "__main__":
    unittest.main(verbosity=2)
