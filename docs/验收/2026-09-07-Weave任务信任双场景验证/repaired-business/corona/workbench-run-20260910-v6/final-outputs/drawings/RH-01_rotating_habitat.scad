/*
 * RH-01_rotating_habitat.scad
 * 日冕计划 Pre-Phase A · 旋转居住舱概念模型（路线 C 关联；口径与 FC-01 一致）
 *
 * 基线: corona-prephase-a-v1 (v1.0.0, frozen_for_validation)
 * 关键参数（与 model/params.json / corona_model.py 一致）:
 *   radius_m   = 1000   -> 概念半径 1000 m
 *   rotation_rpm = 2    -> 2 rpm
 *   a = (2*pi*rpm/60)^2 * r = 43.865 m/s^2 = 4.473 g0
 *   rpm(1 g0) = sqrt(g0/r)*60/(2*pi) = 0.946 rpm
 *
 * 注意: 本模型为比例示意，非真实尺寸；真实半径 1000 m 已按 CONCEPT_SCALE 缩小到 mm 级可视。
 * 概念级 · 不可用于施工 / 制造 / 飞行认证。
 *
 * 编辑方法: 修改下方 CONCEPT_* 参数即可重新生成（OpenSCAD 打开后按 F6 渲染、F7 导出 STL）。
 */

// ---- 概念缩放：1000 m 概念半径 + 缩小至 ~50 mm 便于显示 ----
CONCEPT_RING_RADIUS   = 50;      // 概念环半径 (mm)——示意，非真实尺寸
CONCEPT_RING_TUBE_R   = 8;       // 环管径 (mm)
CONCEPT_HUB_RADIUS    = 12;      // 中心轮毂半径 (mm)
CONCEPT_STRUTS        = 8;       // 辐条数量
CONCEPT_HABITAT_SAIL  = true;    // 是否显示隔热/接口舱段（示意）

$fn = 96;

// 参数注记（仅视觉标注，供审阅；非物理尺寸）
module notes() {
    // 无 3D 文本时以横幅形式给出说明
    echo("CONCEPT: RH-01 rotating habitat, concept-level; r=1000m, 2rpm -> 4.473 g0; 1g0 -> 0.946rpm");
}

module habitat_ring() {
    // 主体：圆环（Torus 概念近似）
    rotate_extrude(convexity = 10)
        translate([CONCEPT_RING_RADIUS, 0, 0])
            circle(r = CONCEPT_RING_TUBE_R);
}

module hub() {
    sphere(r = CONCEPT_HUB_RADIUS);
}

module struts(count) {
    for (i = [0 : count - 1]) {
        rotate([0, 0, i * 360 / count])
            translate([0, 0, 0])
            rotate([90, 0, 0])
            cylinder(r = CONCEPT_RING_TUBE_R * 0.35,
                     h = CONCEPT_RING_RADIUS,
                     center = false);
    }
}

module dwell_section() {
    // 示意居住/载荷舱段：附着于环上（概念）
    translate([CONCEPT_RING_RADIUS + CONCEPT_RING_TUBE_R,
               0, CONCEPT_RING_TUBE_R * 0.4])
        cube([12, 10, 6], center = true);
}

module assembly() {
    notes();
    color("SteelBlue") habitat_ring();
    color("SlateGray") hub();
    color("BurlyWood") struts(CONCEPT_STRUTS);
    if (CONCEPT_HABITAT_SAIL)
        color("OliveDrab") dwell_section();
}

assembly();

// 概念级标注层（供审阅/导出时可见的注释层）
/*
 * 概念级图纸 RH-01 · 版本 v1.0.0
 * 基线 corona-prephase-a-v1 · 速度轴 0.01/0.03/0.05c
 * 数字工程构建器 · 2026-09-09
 * 不可用于施工 / 制造 / 飞行认证
 */
