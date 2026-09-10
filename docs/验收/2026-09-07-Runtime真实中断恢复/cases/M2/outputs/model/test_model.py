#!/usr/bin/env python3
"""test_model.py — 统一计算模型自动测试（有界，自行退出）。

追溯键: baseline_id = corona-baseline-1.0.0 · baseline_version = 1.0.0
        content_digest = cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2

覆盖任务书 §4.1 八类计算 + acceptance.json 参考值容差（RQ-MDL-002/003/005/006/009/011）
+ 追溯键一致性（RQ-MDL-001 / ICD-SOT-003）。

运行方式（从 outputs/model 目录）:
    python3 test_model.py
退出码: 0 = 全部通过；非 0 = 存在失败。
"""
import hashlib
import json
import math
import os
import unittest

import corona_model as cm

HERE = os.path.dirname(os.path.abspath(__file__))


class CoronaModelTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.params = cm.load_params()
        cls.results = cm.full_run(cls.params)
        cls.c = cls.params["constants"]["speed_of_light_m_s"]
        cls.g0 = cls.params["constants"]["standard_gravity_m_s2"]
        cls.dist = cls.params["constants"]["proxima_distance_ly"]
        cls.beta = cls.params["scenarios"]["cruise_speed_c"][0]
        cls.checks = cls.params["reference_checks"]

    # -- 追溯键（RQ-MDL-001 / ICD-SOT-003） ----------------------------------

    def test_traceability_digest_matches_baseline_file(self):
        with open(os.path.join(HERE, "baseline_frozen.yaml"), "rb") as fh:
            digest = hashlib.sha256(fh.read()).hexdigest()
        self.assertEqual(digest, self.params["traceability"]["content_digest"])
        self.assertEqual(digest, "cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2")

    def test_traceability_ids(self):
        self.assertEqual(self.params["traceability"]["baseline_id"], "corona-baseline-1.0.0")
        self.assertEqual(self.params["traceability"]["baseline_version"], "1.0.0")

    def test_scenario_lock_single_003c(self):
        self.assertEqual(self.params["scenarios"]["cruise_speed_c"], [0.03])
        self.assertTrue(self.params["scenarios"]["locked"])

    # -- 参考值容差（acceptance.json reference_checks，RQ-VER-003） ----------

    def _check_within(self, key, actual):
        expected = self.checks[key]["expected"]
        tol = self.checks[key]["relative_tolerance"]
        rel = abs(actual - expected) / abs(expected)
        self.assertLessEqual(rel, tol,
                             "%s: actual=%r expected=%r rel=%.3g tol=%.3g" % (key, actual, expected, rel, tol))

    def test_reference_travel_time(self):
        self._check_within("travel_years_at_0_03c", self.results["reference_checks"]["travel_time_yr"])

    def test_reference_ke_1mt(self):
        self._check_within("kinetic_energy_1mt_at_0_03c_j", self.results["reference_checks"]["kinetic_energy_1mt_j"])

    def test_reference_ke_5mt(self):
        self._check_within("kinetic_energy_5mt_at_0_03c_j", self.results["reference_checks"]["kinetic_energy_5mt_j"])

    def test_reference_artificial_gravity(self):
        self._check_within("gravity_1km_2rpm_m_s2", self.results["reference_checks"]["artificial_gravity_m_s2"])

    def test_reference_dust(self):
        self._check_within("dust_1mg_0_03c_j", self.results["reference_checks"]["dust_impact_energy_j"])

    # -- 任务书 §4.1 八类计算覆盖（RQ-MDL-002..009） -------------------------

    def test_1_travel_time_and_light_time(self):
        self.assertAlmostEqual(cm.travel_time_yr(4.25, 0.03), 141.66666666666669, places=9)
        self.assertAlmostEqual(cm.light_travel_time_yr(4.25), 4.25, places=12)
        self.assertAlmostEqual(self.results["reference_checks"]["travel_time_yr"], 141.6666667, places=6)

    def test_2_nonrel_vs_rel_kinetic_energy(self):
        ke = cm.kinetic_energy_j(1e9, 0.03, self.c)
        ke_rel = cm.kinetic_energy_relativistic_j(1e9, 0.03, self.c)
        self.assertGreater(ke_rel, ke)
        self.assertLess((ke_rel - ke) / ke, 0.001)  # 0.03c 处相对论修正约 0.07%
        self.assertAlmostEqual(ke, 4.04439830431568e22, delta=4.04439830431568e22 * 1e-9)

    def test_3_accel_decel_efficiency_mass_scenarios(self):
        for profile in self.results["accel_decel_scenarios"]:
            a = profile["accel_m_s2"]
            expect_t = (self.beta * self.c) / a / cm.SECONDS_PER_YEAR
            self.assertAlmostEqual(profile["time_to_cruise_yr"], expect_t, places=9)
            self.assertGreater(profile["accel_distance_ly"], 0.0)
        for esc in self.results["efficiency_scenarios"]:
            self.assertAlmostEqual(
                esc["beam_energy_1mt_j"],
                self.results["reference_checks"]["kinetic_energy_1mt_j"] / esc["efficiency"],
                delta=1.0)
        self.assertAlmostEqual(
            self.results["reference_checks"]["kinetic_energy_5mt_j"]
            / self.results["reference_checks"]["kinetic_energy_1mt_j"],
            5.0, places=12)

    def test_4_artificial_gravity(self):
        self.assertAlmostEqual(cm.artificial_gravity_m_s2(1000.0, 2.0), 43.864908449286034, places=9)
        rpm1g = cm.rpm_for_gravity(1000.0, self.g0)
        self.assertAlmostEqual(rpm1g, 0.9457, places=3)  # 纪要 §二.3：约 0.95 rpm
        self.assertAlmostEqual(cm.artificial_gravity_m_s2(1000.0, rpm1g), self.g0, places=6)

    def test_5_dust_impact(self):
        e = cm.dust_impact_energy_j(1e-6, 0.03, self.c)
        self.assertAlmostEqual(e, 40443983.043156795, delta=40443983.043156795 * 1e-9)
        tnt = cm.tnt_equivalent_kg(e, self.params["tnt_equivalent_j_per_kg"]["value"])
        self.assertAlmostEqual(tnt, 9.7, delta=0.2)  # 纪要 §二.4：约 9.7 kg TNT
        # 已撤回数值不得复现（RQ-MDL-006）
        self.assertNotAlmostEqual(e, 4.5e8, delta=1e7)

    def test_6_turn_angle_limits(self):
        tc = self.results["turn_capability"]
        self.assertAlmostEqual(tc["turn_limit_deg_at_10pct_dv"], math.degrees(0.10), places=6)
        self.assertAlmostEqual(tc["turn_limit_deg_at_15pct_dv"], math.degrees(0.15), places=6)
        self.assertAlmostEqual(tc["turn_limit_deg_at_10pct_dv"], 5.7, delta=0.1)
        self.assertAlmostEqual(tc["turn_limit_deg_at_15pct_dv"], 8.6, delta=0.1)
        self.assertFalse(tc["midcourse_target_switch_feasible"])
        self.assertEqual(tc["target_separations"]["proxima_barnard_deg"], 78.0)
        self.assertEqual(tc["target_separations"]["proxima_tau_ceti_deg"], 101.0)
        self.assertEqual(tc["target_separations"]["barnard_tau_ceti_deg"], 117.0)

    def test_7_precursor_probe_timeline(self):
        pp = self.results["precursor_probe"]
        self.assertEqual(pp["cruise_speed_c"], 0.2)
        self.assertAlmostEqual(pp["one_way_flight_yr"], 21.25, places=9)
        self.assertAlmostEqual(pp["data_return_yr"], 4.25, places=9)
        self.assertAlmostEqual(pp["earliest_data_arrival_yr"], 25.5, places=9)

    def test_8_route_sensitivity(self):
        rs = self.results["route_sensitivity"]
        self.assertEqual(rs["locked_scenario_c"], 0.03)
        routes = rs["routes"]
        self.assertEqual(set(routes.keys()), {
            "laser_sail_precursor", "uncrewed_civilization_archive", "crewed_interstellar_vehicle"})
        self.assertAlmostEqual(
            routes["crewed_interstellar_vehicle"]["kinetic_energy_lower_bound_j"],
            self.results["reference_checks"]["kinetic_energy_1mt_j"], delta=1.0)
        self.assertAlmostEqual(routes["laser_sail_precursor"]["one_way_flight_yr"], 21.25, places=9)
        self.assertIn("CCR-001", rs["sensitivity"]["cross_speed_comparison"])

    # -- 情景报告（应用共用入口，RQ-APP-006 同源公式） -------------------------

    def test_scenario_report_baseline_values(self):
        rep = cm.scenario_report(self.params, mass_kg=1e9, efficiency=0.25,
                                 radius_m=1000.0, rotation_rpm=2.0)
        out = rep["outputs"]
        self._check_within("travel_years_at_0_03c", out["travel_time_yr"])
        self._check_within("kinetic_energy_1mt_at_0_03c_j", out["kinetic_energy_j"])
        self._check_within("gravity_1km_2rpm_m_s2", out["artificial_gravity_m_s2"])
        self._check_within("dust_1mg_0_03c_j", out["dust_impact_energy_j"])
        self.assertAlmostEqual(out["beam_energy_at_efficiency_j"], out["kinetic_energy_j"] / 0.25, delta=1.0)
        self.assertIn("不得用作完整推进能源预算", rep["notes"]["kinetic_energy"])


if __name__ == "__main__":
    suite = unittest.TestLoader().loadTestsFromTestCase(CoronaModelTest)
    runner = unittest.TextTestRunner(verbosity=2)
    result = runner.run(suite)
    raise SystemExit(0 if result.wasSuccessful() else 1)
