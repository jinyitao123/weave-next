// ============================================================================
// 日冕计划 · 可编辑概念模型 (CONCEPT-LEVEL, 非施工/制造/认证模型)
// ----------------------------------------------------------------------------
// 模型号: CAD-HAB-001  版本: 1.0.0  日期: 2026-09-07
// 内容:   载人路线旋转居住舱概念布局（环 + 辐条 + 中枢 + 档案载荷挂点）
// 追溯键: baseline_id=corona-baseline-1.0.0  baseline_version=1.0.0
//         content_digest=cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2
// 参数源: outputs/model/baseline_frozen.yaml → results.json（0.03c 锁定情景）
// 单位:   模型单位 = 10 m（1:10 比例）；注释给出真实米制值
// 关键参数 [verified_fact reference_cases.rotating_habitat]:
//   radius_m = 1000 m → R = 100 模型单位
//   rotation_rpm = 2 rpm → 人工重力 43.8649 m/s^2（≈4.47g）[derived_result]
//   1g 转速 = 0.949 rpm @1km [derived_result]
// 事实标签纪律: 本模型为概念布局 [assumption 结构占位]，不构成结构设计；
//   禁用 construction_ready / manufacturing_ready / flight_certified
// ============================================================================

// ---- 可调参数（与冻结基线 reference_cases.rotating_habitat 对应） ----
R        = 100;   // 环半径 = 1000 m / 10
tube_r   = 8;     // 环管半径 = 80 m（概念占位 [assumption]）
hub_r    = 12;    // 中枢半径 = 120 m（概念占位 [assumption]）
hub_h    = 40;    // 中枢长度 = 400 m（概念占位 [assumption]）
n_spokes = 6;     // 辐条数 [assumption]
spoke_w  = 3;     // 辐条截面 = 30 m（概念占位 [assumption]）
fn       = 72;    // 圆分辨率

// ---- 居住环（旋转段，外缘向心加速度 43.8649 m/s^2 @2rpm） ----
module habitat_ring() {
    color("SteelBlue", 0.9)
    rotate_extrude($fn = fn)
        translate([R, 0, 0])
            circle(r = tube_r, $fn = fn);
}

// ---- 中枢（非旋转段：对接、档案载荷、通信） ----
module hub() {
    color("LightGray")
    cylinder(r = hub_r, h = hub_h, center = true, $fn = fn);
    // 通信天线杆（光行时 4.25 yr，回传链路 [unknown 链路预算]）
    color("Orange")
    translate([0, 0, hub_h/2]) cylinder(r = 0.8, h = 30, $fn = 24);
}

// ---- 辐条 ----
module spokes() {
    for (i = [0 : n_spokes - 1])
        rotate([0, 0, i * 360 / n_spokes])
            translate([hub_r, -spoke_w/2, -spoke_w/2])
                cube([R - hub_r, spoke_w, spoke_w]);
}

// ---- 档案载荷挂点（路线 B，与运输解耦，DRW-ARCH-002 概念） ----
module archive_module() {
    color("Goldenrod")
    translate([0, hub_r + 10, 0])
        cube([20, 16, 24], center = true);  // 200×160×240 m 概念占位 [assumption]
}

// ---- 鞭普尔防护盾（待评估概念 [unknown 通量/面密度]，10km 级仅概念保留） ----
module whipple_shield_concept() {
    color("Tomato", 0.35)
    translate([0, 0, -hub_h/2 - 15])
        cylinder(r = hub_r * 1.5, h = 2, center = true, $fn = fn);
}

habitat_ring();
hub();
spokes();
archive_module();
whipple_shield_concept();

// 装配说明：旋转段（ring+spokes）与非旋转段（hub+archive）之间存在
// 旋转接头/轴承接口——其百年级可靠性为 [unknown]，列入课题包。
echo(str("CAD-HAB-001 v1.0.0 baseline=corona-baseline-1.0.0 digest=cfb12b78...8b10c2"));
echo(str("R_m=", R*10, " rpm=2 -> g=", 43.8649, " m/s^2 [derived_result]"));
