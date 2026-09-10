# ============================================================================
# corona_model/baseline.py — 冻结基线加载与追溯键 (RQ-MDL-001, ICD-SOT-001/002)
# ----------------------------------------------------------------------------
# 追溯键: baseline_id=corona-baseline-1.0.0 baseline_version=1.0.0
#         content_digest=cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2
# 模型全部参数派生自 outputs/model/baseline_frozen.yaml（唯一参数源），
# 并对外暴露追溯键三元组 (baseline_id, baseline_version, content_digest)。
# content_digest 在加载时对文件字节实际计算 SHA-256，并与冻结登记值比对，
# 不一致即拒绝运行（防呆，防止参数源被静默改动）。
# ============================================================================
"""Load the frozen baseline and expose traceability keys."""

from __future__ import annotations

import hashlib
import os

from . import miniyaml

BASELINE_PATH = os.path.join(os.path.dirname(__file__), os.pardir, "baseline_frozen.yaml")

# 冻结登记的 SHA-256（来源：inputs/manifest.json 与 change_control_register.md §1，
# 由本轮真实 shasum -a 256 命令核对一致）[verified_fact]
FROZEN_DIGEST = "cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2"

LOCKED_CRUISE_SPEED_C = 0.03  # 单一批准情景（run_input 锁定）[verified_fact]


class BaselineIntegrityError(RuntimeError):
    """Raised when the baseline file digest does not match the frozen record."""


class Baseline:
    """Typed access to the frozen baseline parameters."""

    def __init__(self, path: str = BASELINE_PATH, verify_digest: bool = True):
        self.path = os.path.abspath(path)
        with open(self.path, "rb") as fh:
            raw = fh.read()
        self.content_digest = hashlib.sha256(raw).hexdigest()
        if verify_digest and self.content_digest != FROZEN_DIGEST:
            raise BaselineIntegrityError(
                f"baseline digest mismatch: computed {self.content_digest}, "
                f"frozen record {FROZEN_DIGEST}"
            )
        self.data = miniyaml.loads(raw.decode("utf-8"))

    # -- traceability ----------------------------------------------------
    @property
    def traceability(self) -> dict:
        """追溯键三元组，须嵌入全部下游产物 (ICD-SOT-003)。"""
        return {
            "baseline_id": self.data["baseline_id"],
            "baseline_version": self.data["baseline_version"],
            "content_digest": self.content_digest,
        }

    # -- parameter accessors --------------------------------------------
    @property
    def constants(self) -> dict:
        c = dict(self.data["constants"])
        c.pop("fact_label", None)
        return c

    @property
    def cruise_scenarios(self) -> list:
        return list(self.data["scenarios"]["cruise_speed_c"])

    @property
    def scenario_locked(self) -> bool:
        return bool(self.data["scenarios"].get("locked", False))

    @property
    def reference_cases(self) -> dict:
        return {case["id"]: case for case in self.data["reference_cases"]}

    @property
    def reference_checks(self) -> dict:
        return dict(self.data["reference_checks"])

    @property
    def routes(self) -> list:
        return list(self.data["routes"])

    @property
    def truth_labels(self) -> list:
        return list(self.data["truth_labels"])

    @property
    def forbidden_claims(self) -> list:
        return list(self.data["forbidden_claims"])

    def assert_scope_lock(self) -> None:
        """RQ-SCP-001：本轮唯一批准情景为 0.03c；发现扩展情景即报错。"""
        scenarios = self.cruise_scenarios
        if scenarios != [LOCKED_CRUISE_SPEED_C]:
            raise BaselineIntegrityError(
                f"scope lock violated: cruise scenarios {scenarios} != [0.03]; "
                "speed expansion requires platform-confirmed CCR-001"
            )
