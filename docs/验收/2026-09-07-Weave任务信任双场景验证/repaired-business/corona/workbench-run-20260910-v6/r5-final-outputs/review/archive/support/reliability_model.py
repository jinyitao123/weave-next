# -*- coding: utf-8 -*-
"""
日冕计划 Pre-Phase A · 文明档案载荷 · 长期可靠性与数据保存模型
运行： python3 reliability_model.py
用途：为 archive-reliability-designer 节点"长期可靠性 / 数据保存"提供可复算的数量级依据。
口径：Level-0 概念先期论证。本模型只做【模型推导(derived_result)】与【假设(assumption)】，
      不引入与冻结基线冲突的常量；所有新参数均标注来源与真值标签。
注意：GCR 通量、介质保留期、尘埃通量等为 assumption / unknown，需独立核验；不作为 A 级采购依据。

v6 复用说明：本模型为 v6 节点复核并复用冻结 v4 成果（v4-outputs/review/archive/support/
reliability_model.py）。v6 复核仅移除一处从不被调用、且引用未定义变量 P_none 的死代码函数
ecc_match()；除此之外全文未改。重跑后输出与 v4 证据文件逐字节一致（77 行），未改变任何
结论/派生量/假设值。运行： python3 reliability_model.py
"""
import math

# ============ 冻结常量（fusion, from baseline.yaml, verified_fact） ============
C_LIGHT   = 299792458.0     # m/s
SEC_YEAR  = 31557600.0      # s   (365.25 d)
LY_M      = 9.4607304725808e15  # m
PROXIMA   = 4.25            # ly
G0        = 9.80665         # m/s^2

# ============ 冻结派生量（from baseline.yaml 参考检查 / 01 §2.3, derived_result） ============
def time_alpha(c):
    """到比邻星(4.25 ly)航时 [yr]，匀速、未计加减速。"""
    return PROXIMA * LY_M / (c * C_LIGHT) / SEC_YEAR

SPEEDS = {'S-0.01c': 0.01, 'S-0.03c': 0.03, 'S-0.05c': 0.05}

# ============ 档案载荷长期可靠性模型 ============
# 以下为【假设/量级】参数，需独立核验，非 A 级事实。
D0_GCR_GY_PER_YR = 0.20      # 无屏蔽深空 GCR 电离剂量率 [Gy/yr]（assumption, B级，量级示意）
# 屏蔽对 GCR 的简化衰减：以铝当量面密度 [g/cm^2] 为变量，指数示意。
# 注：GCR 屏蔽并非简单指数（含次级粒子），此处仅为数量级示意，assumption。
def shield_transmit(areal_density_g_cm2, lam=90.0):
    """典型参数：GCR 有效衰减长度 ~90 g/cm^2（assumption, 示意，非实测）。"""
    return math.exp(-areal_density_g_cm2 / lam) if areal_density_g_cm2 >= 0 else 1.0

def dose_T(T, areal=0.0):
    """累计电离剂量 [Gy]；areal=铝当量面密度 [g/cm^2]。"""
    return D0_GCR_GY_PER_YR * T * shield_transmit(areal)

# ---- 介质被动保留期（名义无维护保留寿命 R [yr]）与辐射敏感系数 ----
# 均为 assumption / 量级，需按选定介质供应商数据独立核验。
MEDIA = {
    # key : (R_years_nominal, 描述, 真值标签)
    'analog_etched_metal': (1_000_000.0, '模拟蚀刻金属/钻石(类旅行者金唱片/硅蚀刻)；辐射免疫、被动无电、密度最低', 'assumption'),
    'archival_optical_m' : (    300.0,       'M-DISC 类单次刻录光存；超高记录密度需主动机读', 'assumption'),
    'nand_flash'        : (     30.0,        'NAND 固态；高密度、需刷新/加电、TID 敏感', 'assumption'),
    'magnetic_tape'     : (     50.0,        '磁带/HDD；运动部件、需环境控制、TID 敏感', 'assumption'),
}

def per_copy_survival(media_key, T, areal=0.0):
    """单副本在 T 年后的存活概率 p(T)=exp(-T/R_eff)。
       R_eff 计入辐射剂量加速老化（简化线性耦合，assumption）。"""
    R = MEDIA[media_key][0]
    dose = dose_T(T, areal)
    # 辐射加速系数：每 Gy 等效折减保留期（假设 0.1%/Gy，示意）
    rad_factor = max(1.0, 1.0 + 0.001 * dose)
    R_eff = R / rad_factor
    p = math.exp(-T / R_eff)
    return p, R_eff, dose

def k_of_n_survival(p, N, K):
    """K-of-N 冗余：至少 K 个副本存活（副本独立）时的存档存活概率。"""
    return sum(math.comb(N, j) * (p**j) * ((1-p)**(N-j)) for j in range(K, N+1))

def main():
    print("="*78)
    print("A. 冻结派生量（baseline v1.0.0, derived_result）")
    print("="*78)
    for sid, c in SPEEDS.items():
        print(f"  {sid}:  v={c*C_LIGHT:9.3f} km/s   TIME_ALPHA(比邻星)={time_alpha(c):9.3f} yr")

    print()
    print("="*78)
    print("B. 累计辐射电离剂量（assumption, 量级示意, 需独立核验）")
    print("   D0_GCR=0.20 Gy/yr(无屏蔽), 屏蔽为铝当量面密度指数示意")
    print("="*78)
    for sid, c in SPEEDS.items():
        T = time_alpha(c)
        for areal, name in [(0, '无屏蔽'), (10, '10 g/cm² Al'), (30, '30 g/cm² Al')]:
            print(f"  {sid}  T={T:7.2f} yr  屏蔽{name:>16}:  D={dose_T(T,areal):7.2f} Gy")

    print()
    print("="*78)
    print("C. 介质 × 速度：单副本存活概率 p(T) 与等效保留期 R_eff")
    print("="*78)
    for sid, c in SPEEDS.items():
        T = time_alpha(c)
        print(f"  --- {sid}  T={T:7.2f} yr ---")
        for mk, (R, desc, lab) in MEDIA.items():
            p, Reff, dose = per_copy_survival(mk, T, areal=0.0)
            print(f"    {mk:22s} R_nom={R:10.0f} yr  R_eff={Reff:11.3f} yr  dose={dose:6.2f} Gy  p(T)={p:10.6f}")

    print()
    print("="*78)
    print("D. 冗余(K-of-N) 对存档存活概率(单副本机率 p) 的放大（副本独立假设）")
    print("="*78)
    print(" 设计目标 P_target >= 0.99 覆盖 T(S-0.05c)=85 yr 到 T(S-0.01c)=425 yr")
    for pval in [0.5, 0.7, 0.85, 0.90, 0.95, 0.98, 0.99]:
        line = f"  p(T)={pval:.2f}: "
        for (N,K) in [(3,2),(5,3),(7,5),(10,7),(12,9),(16,12)]:
            P = k_of_n_survival(pval, N, K)
            line += f"{N}-of-{K}={P:.4f}  "
        print(line)

    print()
    print("="*78)
    print("E. 满足 >=0.99 的冗余组合(单副本 p 与 (N,K) 组合)")
    print("="*78)
    rows=[]
    for pval in [0.70,0.80,0.85,0.90,0.95,0.98,0.99]:
        best=None
        for N in [3,4,5,6,8,10,12,16,20]:
            for K in range(1, N+1):
                P = k_of_n_survival(pval, N, K)
                if P >= 0.99:
                    margin = P-0.99
                    key=(margin, -K, N)
                    if best is None or key<best[0]:
                        best=(key, N, K, P)
        if best:
            _,N,K,P=best
            print(f"  p(T)={pval:.2f} -> 最小 {N}-of-{K}  P={P:.4f} (冗余度 {N-K})")
            rows.append((pval,N,K,P))

    print()
    print("="*78)
    print("F. 数据完整性：ECC 未检出/未纠正错误（每副本 BER=1e-10, 1e-12, 1e-14）")
    print("   数据总量 1 PB = 1e16 bit（assumption），ECC 冗余 10%")
    print("="*78)
    for ber in [1e-10, 1e-12, 1e-14]:
        bits = 1e16
        e = bits * ber   # 期望未防护比特错误
        # 无纠错的灾变概率近似：期望错误>0 即高概率存在错误
        print(f"  BER={ber:.0e}: 期望错误比特={e:.2f} (无 ECC 则近乎必然含错)")

    print()
    print("="*78)
    print("G. 尘埃/微流星体暴露窗口（flux 未知 -> 仅给暴露窗口与影响性质）")
    print("="*78)
    for sid, c in SPEEDS.items():
        T = time_alpha(c)
        # 1mg 尘埃撞击能（frozen derived）
        E = 0.5 * 1e-6 * (c*C_LIGHT)**2
        print(f"  {sid}: 暴露窗口={T:7.2f} yr  1mg 尘埃撞击能={E:.3e} J (frozen) | 尘埃通量=unknown，缺直接证据")

if __name__ == "__main__":
    main()
