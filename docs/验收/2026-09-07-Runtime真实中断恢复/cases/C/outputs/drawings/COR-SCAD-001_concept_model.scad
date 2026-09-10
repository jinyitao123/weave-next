// ============================================================================
// 日冕计划 · 概念级三维示意模型 (OpenSCAD)
// 模型号: COR-SCAD-001  版本: 1.0.0
// 等级: 概念级 (Level-0 / Pre-Phase A) — 非施工图、非制造图、非认证设计
// 追溯键: baseline_id=corona-baseline-1.0.0  baseline_version=1.0.0
//         content_digest=cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2
// 参数唯一来源: outputs/model/baseline_frozen.yaml (人工转录, 数值见下, 单位 m, 显示 1:1 概念比例)
//   - rotating_habitat: radius_m=1000, rotation_rpm=2  → 43.8649 m/s^2 [derived_result]
//   - cruise_speed_c=0.03 (锁定单一情景)
// 用途: 载人路线旋转居住环 + 无人档案载荷 + 光帆先锋探测器的概念级并置示意;
//       三路线可分离, 本模型的并置不表示联合构型或可行性外推。
// ============================================================================

// ---------------- 参数 (派生自冻结基线, 转录值见注释) ----------------
habitat_radius_m   = 1000;   // 基线 rotating_habitat.radius_m [verified_fact]
habitat_rpm        = 2;      // 基线 rotating_habitat.rotation_rpm [verified_fact]
ring_section_m     = 60;     // 环截面示意尺寸 [assumption] — 基线未定义, 仅可视化
spoke_count        = 8;      // 辐条数示意 [assumption]
hub_radius_m       = 80;     // 中心毂示意 [assumption]
archive_length_m   = 300;    // 档案载荷示意长 [assumption] — 基线未定义 [unknown]
archive_radius_m   = 40;     // 档案载荷示意半径 [assumption]
sail_side_m        = 300;    // 光帆示意边长 [assumption] (克级探测器帆面概念)
shield_gap_m       = 200;    // 鞭普尔盾间距示意 [assumption] — 十千米级盾列为待评估概念

// 派生校核量 (供 echo 输出, 与模型一致):
// omega = 2*pi*rpm/60; a = omega^2 * r  → 43.8649 m/s^2
omega = 2 * PI * habitat_rpm / 60;
a_centripetal = omega * omega * habitat_radius_m;
echo("CHECK artificial_gravity_m_s2 =", a_centripetal);  // 期望 43.8649 [derived_result]
echo("CHECK rpm_for_1g_at_1km =", 60/(2*PI)*sqrt(9.80665/habitat_radius_m)); // 期望 ≈0.9457

// ---------------- 路线 3: 载人飞行器旋转居住环 (概念示意) ----------------
module rotating_habitat() {
    color("orange", 0.9) {
        // 居住环
        rotate_extrude($fn=96)
            translate([habitat_radius_m, 0, 0])
                circle(r=ring_section_m, $fn=32);
        // 中心毂
        cylinder(h=2*ring_section_m, r=hub_radius_m, center=true, $fn=48);
        // 辐条
        for (i = [0:spoke_count-1])
            rotate([0, 0, i*360/spoke_count])
                translate([hub_radius_m, -8, -8])
                    cube([habitat_radius_m - hub_radius_m, 16, 16]);
    }
}

// ---------------- 路线 2: 无人文明档案载荷 (概念示意) ----------------
module archive_payload() {
    color("green", 0.85) {
        cylinder(h=archive_length_m, r=archive_radius_m, center=true, $fn=48); // 主存储舱
        translate([0, 0, archive_length_m/2 + shield_gap_m/2])
            cylinder(h=4, r=archive_radius_m*1.6, center=true, $fn=48);        // 前盾 (概念)
        translate([0, 0, archive_length_m/2 + shield_gap_m])
            cylinder(h=4, r=archive_radius_m*1.9, center=true, $fn=48);        // 二盾 (待评估概念)
    }
}

// ---------------- 路线 1: 激光光帆先锋探测器 (概念示意) ----------------
module sail_precursor() {
    color("steelblue", 0.7)
        cube([sail_side_m, sail_side_m, 1], center=true);  // 帆面
    color("black")
        cube([10, 10, 10], center=true);                   // 克级载荷芯片示意 (不按比例)
}

// ---------------- 并置布局 (仅展示用, 非联合构型) ----------------
translate([0, 0, 0])             rotating_habitat();   // 路线 3
translate([habitat_radius_m + 600, 0, 0]) archive_payload(); // 路线 2
translate([-(habitat_radius_m + 700), 0, 0]) sail_precursor(); // 路线 1

// 注: 图形不按任务比例; 所有非基线尺寸均标注 [assumption]/[unknown],
//     概念级边界见 outputs/drawings 各 SVG 图签与 interface_control.md。
