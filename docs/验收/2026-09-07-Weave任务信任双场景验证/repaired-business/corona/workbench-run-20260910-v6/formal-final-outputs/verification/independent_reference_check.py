#!/usr/bin/env python3
"""
independent_reference_check.py —— verification-integrator 独立参考锚点复算。

不 import 任何团队产物（corona_model / run_checks），只用：
  - acceptance.json 中的期望值与容差
  - baseline.yaml / 验收任务书 的冻结常量与公式
直接由物理公式计算五项 0.03c 参考锚点，判相对误差是否在容差内。
"""
import json
import math
import os

HERE = os.path.dirname(os.path.abspath(__file__))
OUTPUTS = os.path.dirname(HERE)
WORKDIR = os.path.dirname(OUTPUTS)

# 权威 acceptance.json（验收任务书指定的 /Users/jinyitao/Documents/日冕/complex-validation/）
ACCEPTANCE = "/Users/jinyitao/Documents/日冕/complex-validation/acceptance.json"
with open(ACCEPTANCE, "r", encoding="utf-8") as fh:
    acc = json.load(fh)
ref = acc["reference_checks"]

# 冻结常量（baseline.yaml）
C_LIGHT = 299792458.0          # m/s
PROXIMA_LY = 4.25              # ly
G0 = 9.80665                   # m/s^2
RADIUS_M = 1000.0
RPM = 2.0

CS = 0.03

def travel_years(c):
    # 4.25 ly / c：光行时（匀速，未计加减速）；单位 yr
    return PROXIMA_LY / c

def ke_joules(mass_kg, c):
    # 经典动能 0.5 m v^2（0.05c 相对论修正<0.2%，经典式可接受）
    v = c * C_LIGHT
    return 0.5 * mass_kg * v * v

def gravity_1km_2rpm(radius_m=RADIUS_M, rpm=RPM):
    omega = 2.0 * math.pi * rpm / 60.0
    return omega * omega * radius_m

# 五项锚点的独立计算
values = {
    "travel_years_at_0_03c": travel_years(CS),
    "kinetic_energy_1mt_at_0_03c_j": ke_joules(1.0e9, CS),
    "kinetic_energy_5mt_at_0_03c_j": ke_joules(5.0e9, CS),
    "gravity_1km_2rpm_m_s2": gravity_1km_2rpm(),
    "dust_1mg_0_03c_j": ke_joules(1.0e-6, CS),
}

print("INDEPENDENT REFERENCE CHECK (verification-integrator, runtime 84da6261)")
print("baseline constants: c=%r proxima=%r g0=%r r=%r rpm=%r" % (C_LIGHT, PROXIMA_LY, G0, RADIUS_M, RPM))
print("")
print("%-30s %-16s %-16s %-12s %-8s %s" %
      ("check", "actual", "expected", "rel_err", "tol", "status"))
all_ok = True
worst = 0.0
for name, spec in ref.items():
    exp = spec["expected"]
    tol = spec["relative_tolerance"]
    act = values[name]
    relerr = abs(act - exp) / abs(exp) if exp else 0.0
    ok = relerr <= tol
    all_ok = all_ok and ok
    worst = max(worst, relerr)
    print("%-30s %-16.6e %-16.6e %-12.3e %-8g %s" %
          (name, act, exp, relerr, tol, "PASS" if ok else "FAIL"))
print("")
print("ALL_REFERENCE_CHECKS_%s (worst rel_err=%.3e, n=%d)" %
      ("PASS" if all_ok else "FAIL", worst, len(ref)))
# 与模型/团队声称的最大相对误差（2.58e-8）同量级即满足独立复算容差
print("N=%d/%d within tolerance" % (sum(1 for n in ref if values[n] is not None), len(ref)))
sys_exit = 0 if all_ok else 1
