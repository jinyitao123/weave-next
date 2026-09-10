# ============================================================================
# tests/test_physics.py — 规范公式单元测试 (ICD-FML-001..005)
# 追溯键: baseline_id=corona-baseline-1.0.0 baseline_version=1.0.0
#         content_digest=cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2
# ============================================================================
import math
import os
import sys
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), os.pardir))

from corona_model import physics

C = 299792458.0


class TestFormulas(unittest.TestCase):
    def test_travel_time_icd_fml_001(self):
        self.assertAlmostEqual(physics.travel_time_yr(4.25, 0.03), 141.66666666666669)
        with self.assertRaises(ValueError):
            physics.travel_time_yr(4.25, 0.0)

    def test_light_travel_time(self):
        self.assertEqual(physics.light_travel_time_yr(4.25), 4.25)

    def test_kinetic_energy_classical_icd_fml_002(self):
        ke = physics.kinetic_energy_classical_j(1e9, 0.03, C)
        self.assertAlmostEqual(ke, 4.04439830431568e22, delta=4.04439830431568e22 * 1e-12)

    def test_kinetic_energy_relativistic_contrast(self):
        """相对论对照 (γ-1)mc² 应略高于非相对论值（0.03c 下约 0.07%）。"""
        ke_cl = physics.kinetic_energy_classical_j(1e9, 0.03, C)
        ke_rel = physics.kinetic_energy_relativistic_j(1e9, 0.03, C)
        frac = (ke_rel - ke_cl) / ke_cl
        self.assertGreater(frac, 0.0)
        self.assertLess(frac, 0.002)
        self.assertAlmostEqual(ke_rel, 4.0471e22, delta=4.0471e22 * 0.001)

    def test_lorentz_gamma_bounds(self):
        self.assertAlmostEqual(physics.lorentz_gamma(0.0), 1.0)
        self.assertAlmostEqual(physics.lorentz_gamma(0.03), 1.0004503039763835, places=12)
        with self.assertRaises(ValueError):
            physics.lorentz_gamma(1.0)

    def test_artificial_gravity_icd_fml_003(self):
        a = physics.artificial_gravity_m_s2(1000.0, 2.0)
        self.assertAlmostEqual(a, 43.864908449286034, places=9)
        self.assertAlmostEqual(a / 9.80665, 4.4730, places=3)

    def test_rpm_for_1g(self):
        """纪要 §二.3：同一半径取得 1g 转速约 0.95 rpm。"""
        rpm = physics.rpm_for_target_gravity(1000.0, 9.80665)
        self.assertAlmostEqual(rpm, 0.9455, places=3)

    def test_dust_impact_icd_fml_004(self):
        e = physics.dust_impact_energy_j(1e-6, 0.03, C)
        self.assertAlmostEqual(e, 40443983.043156795, delta=40443983.043156795 * 1e-12)
        self.assertAlmostEqual(physics.tnt_equivalent_kg(e), 9.666, places=2)
        e10 = physics.dust_impact_energy_j(1e-5, 0.03, C)
        self.assertAlmostEqual(e10, 10 * e, delta=1e-3 * e)

    def test_turn_angle_limit_icd_fml_005(self):
        """纪要 §二.5：10%–15% Δv 仅支持约 5.7°–8.6° 转向。"""
        self.assertAlmostEqual(physics.turn_angle_limit_deg(0.10), 5.7296, places=3)
        self.assertAlmostEqual(physics.turn_angle_limit_deg(0.15), 8.5944, places=3)

    def test_precursor_return(self):
        r = physics.precursor_earliest_return_yr(4.25, 0.2)
        self.assertAlmostEqual(r["flight_time_min_yr"], 21.25)
        self.assertAlmostEqual(r["data_return_yr"], 4.25)
        self.assertAlmostEqual(r["earliest_return_yr"], 25.5)

    def test_propulsion_energy_lower_bound(self):
        ke = 4.04439830431568e22
        self.assertAlmostEqual(physics.propulsion_energy_lower_bound_j(ke, 1.0), ke)
        self.assertAlmostEqual(physics.propulsion_energy_lower_bound_j(ke, 0.5), 2 * ke)
        with self.assertRaises(ValueError):
            physics.propulsion_energy_lower_bound_j(ke, 0.0)
        with self.assertRaises(ValueError):
            physics.propulsion_energy_lower_bound_j(ke, 1.5)


if __name__ == "__main__":
    unittest.main()
