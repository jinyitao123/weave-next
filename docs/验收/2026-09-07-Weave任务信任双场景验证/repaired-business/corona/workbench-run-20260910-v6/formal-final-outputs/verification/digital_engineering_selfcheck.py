#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
digital_engineering_selfcheck.py
数字工程节点（digital-engineering）· 图纸侧自检（drawings/）

检查内容（对应验收门槛 G6/G7 的图纸侧逻辑）：
  G6 图纸可解析且可编辑（SVG 为合法 XML；SCAD 为可编辑脚本）
  G7 全部图纸标注概念级（含「不可用于施工/制造/飞行认证」类措辞）
  FD-01 修正脚注保留

约束：
  - 所有计数（SVG 张数、SCAD 数、概念级标注命中数、FD-01 脚注）由实际文件扫描动态得出。
  - 有界退出：全部通过 exit 0；任一失败 exit 1。
  - 仅读取本节点 outputs/ 下交付的 drawings/，不修改任何文件。

运行：python3 outputs/verification/digital_engineering_selfcheck.py
"""
import os, re, sys
import xml.etree.ElementTree as ET

HERE = os.path.dirname(os.path.abspath(__file__))
DRAWINGS = os.path.normpath(os.path.join(HERE, "..", "drawings"))

ok, bad = [], []
def assert_(cond, msg):
    (ok if cond else bad).append(msg)

# ---- 扫描（动态计数）----
names = sorted(os.listdir(DRAWINGS))
svg_files = [n for n in names if n.lower().endswith(".svg")]
scad_files = [n for n in names if n.lower().endswith(".scad")]
other_files = [n for n in names if not (n.lower().endswith(".svg") or n.lower().endswith(".scad"))]
assert_(len(svg_files) >= 3, "SVG 张数（动态扫描，至少 3）: %d" % len(svg_files))
assert_(len(scad_files) == 1, "OpenSCAD 模型数（动态扫描，==1）: %d" % len(scad_files))

# ---- G6 SVG 可解析（合法 XML 且为 <svg> 根）----
for n in svg_files:
    p = os.path.join(DRAWINGS, n)
    try:
        tree = ET.parse(p)
        root = tree.getroot()
        is_svg = root.tag.endswith("svg") or root.tag == "svg"
        viewBox = root.get("viewBox")
        assert_(is_svg and viewBox, "G6 %s 解析为合法 SVG（<svg> 根, viewBox=%s）" % (n, viewBox))
    except ET.ParseError as e:
        assert_(False, "G6 %s 不是合法 XML: %s" % (n, e))

# ---- G7 全部绘制件标注概念级 ----
CONCEPT_RE = re.compile(r"概念级|不可用于施工|不可用于制造|不可用于飞行|概念")
FD01_FOOTNOTE = "柱值为 1 mt（1e9 kg）质量基线 ½mv² 动能 → 4.49e21–1.12e23 J"
for n in svg_files:
    p = os.path.join(DRAWINGS, n)
    text = open(p, encoding="utf-8").read()
    hits = set(CONCEPT_RE.findall(text))
    assert_(len(hits) > 0, "G7 %s 标注概念级（命中词 %s）" % (n, sorted(hits)))

# ---- FD-01 修正脚注（必须保留）----
fd = os.path.join(DRAWINGS, "FD-01_speed_axis_scenario_comparison.svg")
fd_text = open(fd, encoding="utf-8").read()
assert_(FD01_FOOTNOTE in fd_text, "FD-01 修正脚注保留（精确匹配）")
line_with_foot = None
for i, line in enumerate(fd_text.splitlines(), 1):
    if "柱值为 1 mt" in line:
        line_with_foot = i
        break
assert_(line_with_foot is not None, "FD-01 脚注位于第 %s 行（动态检测）" % line_with_foot)

# ---- SCAD 可编辑（含可编辑参数 + 概念级声明）----
for n in scad_files:
    p = os.path.join(DRAWINGS, n)
    text = open(p, encoding="utf-8").read()
    has_params = bool(re.search(r"^\s*[A-Z_0-9]+\s*=\s*[^;]+;", text, re.M))
    assert_(has_params, "%s 含可编辑 CONCEPT_* 参数声明" % n)

# ---- 汇总（动态计数）----
print("=== digital-engineering 图纸侧自检（drawings/）===")
print("交付件计数（动态）：SVG=%d  SCAD=%d  其他=%s" % (len(svg_files), len(scad_files), other_files or "none"))
print("检查数: %d（动态）  通过: %d  失败: %d" % (len(ok) + len(bad), len(ok), len(bad)))
for line in ok:
    print("  [PASS] " + line)
for line in bad:
    print("  [FAIL] " + line)
if bad:
    print("RESULT: FAIL（%d 项未通过）" % len(bad), file=sys.stderr)
    sys.exit(1)
print("RESULT: PASS（全部 %d 项图纸侧检查通过）" % len(ok))
sys.exit(0)
