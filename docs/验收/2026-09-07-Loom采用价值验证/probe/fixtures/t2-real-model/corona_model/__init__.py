# ============================================================================
# corona_model/__init__.py
# 追溯键: baseline_id=corona-baseline-1.0.0 baseline_version=1.0.0
#         content_digest=cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2
# ============================================================================
"""日冕计划统一计算模型（Level-0 / Pre-Phase A 概念级）。"""

from .baseline import Baseline, BaselineIntegrityError, FROZEN_DIGEST, LOCKED_CRUISE_SPEED_C

__all__ = ["Baseline", "BaselineIntegrityError", "FROZEN_DIGEST", "LOCKED_CRUISE_SPEED_C"]
__version__ = "1.0.0"
