# ============================================================================
# tests/test_computations.py — §4.1 八类计算与 acceptance.json 容差比对
# (RQ-MDL-002/003/005/006/007/008/009, RQ-VER-003)
# 追溯键: baseline_id=corona-baseline-1.0.0 baseline_version=1.0.0
#         content_digest=cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2
# ============================================================================
import os
import sys
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), os.pardir))

from corona_model import Baseline, computations
from run_model import reference_check_results


class TestEightCategories(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.baseline = Baseline()
        cls.records = computations.run_all(cls.baseline)
        cls.by_id = {r["id"]: r for r in cls.records}

    def test_eight_categories_present(self):
        """任务书 §4.1 八类计算全覆盖。"""
        self.assertEqual(len(self.records), 8)
        expected = {
            "travel_and_light_time", "kinetic_energy_comparison",
            "accel_decel_efficiency_mass", "artificial_gravity", "dust_impact",
            "target_geometry_turning", "precursor_return", "route_sensitivity",
        }
        self.assertEqual(set(self.by_id), expected)

    def test_records_carry_rq_mdl_010_fields(self):
        """每条关键数值带单位、公式、输入、输出、来源与事实标签。"""
        for r in self.records:
            for field in ("unit", "formula_ref", "inputs", "outputs", "source", "fact_label"):
                self.assertIn(field, r, f"{r['id']} missing {field}")
            self.assertIn(r["fact_label"], self.baseline.truth_labels)

    def test_travel_time(self):
        out = self.by_id["travel_and_light_time"]["outputs"]
        self.assertAlmostEqual(out["travel_time_yr"], 141.6666667, places=6)
        self.assertAlmostEqual(out["light_travel_time_yr"], 4.25)
        self.assertIn("加速", self.by_id["travel_and_light_time"]["boundary"])

    def test_kinetic_energy_lower_bound_discipline(self):
        rec = self.by_id["kinetic_energy_comparison"]
        self.assertIn("下限", rec["boundary"])
        self.assertIn("不得用作工程能源预算", rec["boundary"])
        out = rec["outputs"]["crewed_1mt"]
        self.assertIn("kinetic_energy_relativistic_j", out)
        self.assertGreater(out["relativistic_minus_classical_fraction"], 0.0)

    def test_efficiency_mass_scenarios_keep_unknowns(self):
        rec = self.by_id["accel_decel_efficiency_mass"]
        self.assertIn("unknown", rec["boundary"])
        self.assertIn("减速", rec["outputs"]["decel_requirement"])

    def test_artificial_gravity(self):
        out = self.by_id["artificial_gravity"]["outputs"]
        self.assertAlmostEqual(out["artificial_gravity_m_s2"], 43.8649, places=3)
        self.assertAlmostEqual(out["rpm_for_1g_same_radius"], 0.95, places=2)

    def test_dust_impact_and_withdrawn_value_absent(self):
        """RQ-MDL-006：撤回值 450MJ 不得复现。"""
        rec = self.by_id["dust_impact"]
        out = rec["outputs"]
        self.assertAlmostEqual(out["dust_1mg_energy_j"], 40443983, delta=40443983 * 0.001)
        self.assertAlmostEqual(out["dust_1mg_tnt_equivalent_kg"], 9.7, delta=0.1)
        import json
        self.assertNotIn("450", json.dumps(rec))

    def test_target_geometry(self):
        out = self.by_id["target_geometry_turning"]["outputs"]
        self.assertAlmostEqual(out["turn_angle_limit_deg_min"], 5.7, delta=0.1)
        self.assertAlmostEqual(out["turn_angle_limit_deg_max"], 8.6, delta=0.1)
        for pair, possible in out["mid_course_target_switch_possible"].items():
            self.assertFalse(possible, f"{pair} 中途切换不应可行")

    def test_precursor_return(self):
        out = self.by_id["precursor_return"]["outputs"]
        self.assertAlmostEqual(out["earliest_return_yr"], 25.5)
        self.assertIn("第 20 年以前", out["decision_constraint"])

    def test_route_sensitivity_separability(self):
        out = self.by_id["route_sensitivity"]["outputs"]
        self.assertEqual(set(out), {"laser_sail_precursor",
                                    "uncrewed_civilization_archive",
                                    "crewed_interstellar_vehicle"})
        # 三路线质量量级差异巨大，但航行时间在同一锁定情景下相同
        times = {rid: spec["travel_time_yr_at_locked_0_03c"] for rid, spec in out.items()}
        self.assertEqual(len(set(times.values())), 1)
        kes = [spec["kinetic_energy_lower_bound_j"] for spec in out.values()]
        self.assertGreater(max(kes) / min(kes), 1e10)  # 量级差体现不可外推性
        self.assertIn("禁止", self.by_id["route_sensitivity"]["boundary"])


class TestReferenceTolerances(unittest.TestCase):
    """acceptance.json reference_checks 容差比对（RQ-VER-003）。"""

    def test_all_five_reference_checks_pass(self):
        baseline = Baseline()
        records = computations.run_all(baseline)
        results = reference_check_results(baseline, records)
        self.assertEqual(len(results), 5)
        for r in results:
            self.assertTrue(
                r["pass"],
                f"{r['check']}: computed={r['computed']} expected={r['expected']} "
                f"rel_dev={r['relative_deviation']} tol={r['relative_tolerance']}",
            )


if __name__ == "__main__":
    unittest.main()
