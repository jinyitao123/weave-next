# ============================================================================
# tests/test_discipline.py — 事实纪律与范围锁定测试
# (RQ-SCP-001/002/003/005, RQ-MDL-004, RQ-APP-007 之模型侧)
# 追溯键: baseline_id=corona-baseline-1.0.0 baseline_version=1.0.0
#         content_digest=cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2
# ============================================================================
import json
import os
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), os.pardir))

from corona_model import Baseline, computations

MODEL_DIR = os.path.join(os.path.dirname(__file__), os.pardir)


class TestScopeDiscipline(unittest.TestCase):
    def test_no_expanded_speed_scenarios_in_categories(self):
        """RQ-SCP-001：八类计算输出不得把 0.01c/0.05c 作为已实现情景。"""
        baseline = Baseline()
        records = computations.run_all(baseline)
        blob = json.dumps(records)
        # CCR-001 仅允许以"待确认变更/预留接口"语境出现；计算输出中不得出现
        # 0.01/0.05 巡航情景字段。
        self.assertNotIn('"cruise_speed_c": 0.01', blob)
        self.assertNotIn('"cruise_speed_c": 0.05', blob)
        self.assertNotIn("0.01c 情景", blob)
        self.assertNotIn("0.05c 情景", blob)

    def test_no_forbidden_claims_in_model_source(self):
        """RQ-SCP-005：模型源码与输出不得出现四类禁用声明（作为肯定性声明）。"""
        baseline = Baseline()
        forbidden = baseline.forbidden_claims
        records = computations.run_all(baseline)
        blob = json.dumps(records, ensure_ascii=False)
        for claim in forbidden:
            self.assertNotIn(claim, blob)

    def test_kinetic_energy_never_called_full_budget(self):
        """RQ-MDL-004：任何输出不得把动能下限表述为完整推进能源预算。"""
        baseline = Baseline()
        records = computations.run_all(baseline)
        blob = json.dumps(records, ensure_ascii=False)
        self.assertNotIn("完整推进能源预算", blob.replace("不得用作工程能源预算", "")
                         .replace("非完整推进能源预算", ""))

    def test_fact_labels_restricted_to_four(self):
        baseline = Baseline()
        allowed = set(baseline.truth_labels)

        def walk(node):
            if isinstance(node, dict):
                for k, v in node.items():
                    if k == "fact_label" or k.endswith("fact_label"):
                        if isinstance(v, str):
                            self.assertIn(v, allowed)
                    walk(v)
            elif isinstance(node, list):
                for item in node:
                    walk(item)

        walk(computations.run_all(baseline))


class TestCLIContract(unittest.TestCase):
    """run_model.py 有界退出契约：成功路径退出码 0 并产出 JSON。"""

    def test_run_model_cli_bounded(self):
        with tempfile.TemporaryDirectory() as tmp:
            app_data = os.path.join(tmp, "app", "data", "baseline_params.json")
            proc = subprocess.run(
                [sys.executable, os.path.join(MODEL_DIR, "run_model.py"),
                 "--out-dir", tmp, "--app-data", app_data],
                capture_output=True, text=True, timeout=60,
            )
            self.assertEqual(proc.returncode, 0, proc.stderr)
            self.assertIn("all reference checks within tolerance", proc.stdout)
            with open(os.path.join(tmp, "model_output.json"), encoding="utf-8") as fh:
                out = json.load(fh)
            self.assertTrue(out["all_reference_checks_pass"])
            self.assertEqual(out["traceability"]["content_digest"], Baseline().content_digest)
            with open(app_data, encoding="utf-8") as fh:
                params = json.load(fh)
            self.assertEqual(params["traceability"]["baseline_id"], "corona-baseline-1.0.0")
            self.assertTrue(params["scope"]["scenario_lock"])
            self.assertEqual(params["scope"]["cruise_speed_c_locked"], 0.03)


if __name__ == "__main__":
    unittest.main()
