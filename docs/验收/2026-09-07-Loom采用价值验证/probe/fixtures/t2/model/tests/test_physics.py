import math
import unittest

from model.physics import cruise_years, kinetic_energy_j, lorentz_gamma


class PhysicsTests(unittest.TestCase):
    def test_lorentz_gamma(self):
        self.assertAlmostEqual(lorentz_gamma(0.03), 1.0004503039763835, places=12)

    def test_cruise_years(self):
        self.assertAlmostEqual(cruise_years(4.25, 0.03), 141.66666666666666)

    def test_kinetic_energy(self):
        expected = 0.5 * 1e9 * (0.03 * 299792458.0) ** 2
        actual = kinetic_energy_j(1e9, 0.03)
        self.assertLess(abs(actual - expected) / expected, 1e-12)


if __name__ == "__main__":
    unittest.main()
