# ============================================================================
# tests/test_baseline.py — 基线加载、摘要核对、情景锁定 (RQ-MDL-001, RQ-SCP-001)
# 追溯键: baseline_id=corona-baseline-1.0.0 baseline_version=1.0.0
#         content_digest=cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2
# ============================================================================
import os
import sys
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), os.pardir))

from corona_model import Baseline, BaselineIntegrityError, FROZEN_DIGEST
from corona_model import miniyaml


class TestBaselineLoading(unittest.TestCase):
    def setUp(self):
        self.baseline = Baseline()

    def test_digest_matches_frozen_record(self):
        """content_digest 实际计算值与冻结登记一致（防静默改动）。"""
        self.assertEqual(self.baseline.content_digest, FROZEN_DIGEST)

    def test_traceability_triple(self):
        t = self.baseline.traceability
        self.assertEqual(t["baseline_id"], "corona-baseline-1.0.0")
        self.assertEqual(t["baseline_version"], "1.0.0")
        self.assertEqual(t["content_digest"], FROZEN_DIGEST)

    def test_scope_lock_single_0_03c(self):
        """RQ-SCP-001：唯一批准情景 0.03c；不得含 0.01c/0.05c。"""
        self.assertEqual(self.baseline.cruise_scenarios, [0.03])
        self.assertTrue(self.baseline.scenario_locked)
        self.baseline.assert_scope_lock()  # 不抛异常
        for s in self.baseline.cruise_scenarios:
            self.assertNotIn(s, (0.01, 0.05))

    def test_constants_inherited(self):
        c = self.baseline.constants
        self.assertEqual(c["speed_of_light_m_s"], 299792458)
        self.assertEqual(c["standard_gravity_m_s2"], 9.80665)
        self.assertEqual(c["proxima_distance_ly"], 4.25)

    def test_reference_cases(self):
        rc = self.baseline.reference_cases
        self.assertEqual(rc["crewed_1mt"]["mass_kg"], 1000000000)
        self.assertEqual(rc["orbital_material_5mt"]["mass_kg"], 5000000000)
        self.assertEqual(rc["dust_1mg"]["mass_kg"], 0.000001)
        self.assertEqual(rc["rotating_habitat"]["radius_m"], 1000)
        self.assertEqual(rc["rotating_habitat"]["rotation_rpm"], 2)

    def test_reference_checks_present(self):
        checks = self.baseline.reference_checks
        self.assertEqual(len(checks), 5)
        self.assertAlmostEqual(checks["travel_years_at_0_03c"]["expected"], 141.6666667)

    def test_routes_separable(self):
        ids = [r["id"] for r in self.baseline.routes]
        self.assertEqual(ids, ["laser_sail_precursor",
                               "uncrewed_civilization_archive",
                               "crewed_interstellar_vehicle"])

    def test_truth_labels_and_forbidden_claims(self):
        self.assertEqual(self.baseline.truth_labels,
                         ["verified_fact", "derived_result", "assumption", "unknown"])
        self.assertEqual(self.baseline.forbidden_claims,
                         ["construction_ready", "manufacturing_ready",
                          "flight_certified", "whole_program_cost_committed"])

    def test_digest_mismatch_rejected(self):
        """篡改参数源必须被拒绝（RQ-MDL-001 防呆）。"""
        import tempfile
        with open(self.baseline.path, "r", encoding="utf-8") as fh:
            text = fh.read()
        tampered = text.replace("299792458", "299792459")
        with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False,
                                         encoding="utf-8") as tmp:
            tmp.write(tampered)
            tmp_path = tmp.name
        try:
            with self.assertRaises(BaselineIntegrityError):
                Baseline(path=tmp_path)
        finally:
            os.unlink(tmp_path)


class TestMiniYAML(unittest.TestCase):
    def test_scalar_and_list_parsing(self):
        doc = miniyaml.loads(
            "a: 1\nb: 2.5\nc: [0.01, 0.03, 0.05]\nd: true\ne: hello world\n"
            "f:\n  g: 10\n  h: x\nlst:\n  - id: one\n    v: 1\n  - id: two\n    v: 2\n"
        )
        self.assertEqual(doc["a"], 1)
        self.assertEqual(doc["b"], 2.5)
        self.assertEqual(doc["c"], [0.01, 0.03, 0.05])
        self.assertIs(doc["d"], True)
        self.assertEqual(doc["e"], "hello world")
        self.assertEqual(doc["f"], {"g": 10, "h": "x"})
        self.assertEqual(doc["lst"], [{"id": "one", "v": 1}, {"id": "two", "v": 2}])

    def test_comments_and_block_scalars(self):
        doc = miniyaml.loads("# comment\na: 5  # trailing\nnote: >-\n  folded text\n  more\nb: 6\n")
        self.assertEqual(doc["a"], 5)
        self.assertEqual(doc["b"], 6)
        self.assertIn("folded text", doc["note"])


if __name__ == "__main__":
    unittest.main()
