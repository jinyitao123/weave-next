# 成员验证日志 · luna-digital-engineering-builder（模型/图纸/应用节点）

- 追溯键: baseline_id=`corona-baseline-1.0.0` · baseline_version=`1.0.0` · content_digest=`cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2`
- 日期: 2026-09-07 · 性质: 本轮独立成员执行复核（新运行，不复用旧运行文件）
- 纪律: 全部命令有界退出（纯本地计算/解析，无前台开发服务器、无网络）；未执行项列 `not_run`，不宣称 PASS。

## 1. 输入核对（Read 工具实际读取）

- `inputs/reference/background.md`、`baseline.yaml`、`task-spec.md`、`acceptance.json`：已读取。
- `inputs/lead/model/baseline_frozen.yaml` + 三份 review 文件：已读取；SHA-256 与 `inputs/manifest.json` 逐一相符（见 `results/digest.log` 对应来源值）。
- 未扫描同机其他成员/旧运行目录；`outputs/model/baseline_frozen.yaml` 与 `outputs/review/*.md` 为 inputs/lead 的字节相同副本（ICD-DLV-001 复制整合），摘要复核见 `results/digest.log`。

## 2. 模型测试（PASS）

- CMD: `cd outputs/model && python3 corona_model.py` → exit=0，写出 `results.json`
- CMD: `cd outputs/model && python3 test_model.py` → **Ran 20 tests … OK, exit=0**
- 原始输出: `results/model_tests.log`
- 覆盖: 追溯链摘要校验、情景锁定、CCR-001 未纳入、acceptance.json 五项参考值容差（全部 PASS）、§4.1 八类计算存在性与自洽性（相对论对照、加减速边界、转向上限 5.73°–8.59°、先锋 25.5 yr、三路线敏感性与可分离注记）。
- 本轮修复记录（真实诊断）:
  1. ruby YAML 1.1 将 `4.0443983e22`（无符号指数）解析为字符串 → 参考值比对 TypeError；修复为 `load_baseline` 统一 float 归一，重跑通过。
  2. 相对论/经典动能比值断言初稿误写 1.000225；正确值 2(γ−1)/β²≈1.0006755，修正断言后通过。
  3. 跨产物检查对 forbidden_claims 语境判定过窄（60 字符窗口/跨行 docstring）→ 改为整行判定并将模型 docstring 收拢单行，重跑通过。

## 3. 应用内核一致性（PASS）

- CMD: `cd outputs/app && node test_core.js` → **11 checks PASS, ALL APP CORE TESTS PASS, exit=0**
- CMD: `node --check corona_core.js` → SYNTAX-OK
- 原始输出: `results/app_core_tests.log`
- 证明: 应用 `corona_core.js` 与 Python 模型 `results.json` 五项参考值逐一相等（rel=0）；情景锁定拒绝 0.05c；三情景比较仅为接口占位；导出 JSON 含追溯键+输入+输出。
- 导出实证: `outputs/app/sample_export_scenario.json` 由 node 实际调用应用内核生成。

## 4. 图纸解析与渲染（PASS）

- CMD: `xmllint --noout *.svg` → 三张 SVG 全部 XML-PARSE-OK, exit=0
- CMD: `openscad --version` → OpenSCAD version 2026.09.03（**本机就绪，DISC-003 消解**）
- CMD: `openscad -o /tmp/cad-hab-001-check.csg CAD-HAB-001_concept_habitat.scad` → exit=0（解析+CSG 导出，ECHO 追溯键正确）
- CMD: `openscad -o /tmp/cad-hab-001-render.png …` → exit=0，CGAL 渲染 0.093s（渲染结果经查看确认：环+中枢+6 辐条+档案舱+防护盾布局正确；PNG 为二进制不作交付物）
- 原始输出: `results/drawings_checks.log`

## 5. 跨产物一致性（PASS）

- CMD: `cd outputs/verification && python3 cross_artifact_check.py` → **27 PASS / 0 FAIL, ALL-PASS, exit=0**
- 原始输出: `results/cross_artifact.log`
- 覆盖: 基线摘要、9 类产物追溯键嵌入、图纸数值=模型结果、三张 SVG 概念级标注、0.01c/0.05c 仅出现于 CCR-001 注记语境、forbidden_claims 整行语境、导出 JSON=模型值。

## 6. 应用本地依赖完整性（PASS，脚本检查）

- `index.html` 的 `<script src>` 依赖 `params.js`、`corona_core.js` 均存在；`window.open` 的三张图纸相对路径 `../drawings/*.svg` 均存在；HTML 解析无误（python3 html.parser）。原始输出见会话记录；结论纳入 §3 覆盖范围。

## 7. NOT_RUN（诚实声明，不得视为 PASS）

1. 浏览器 GUI 交互（人工打开页面、拖动滑块、点击导出）：无浏览器自动化工具 → NOT_RUN（计算/导出结构已由 node 覆盖）。
2. 需求验证矩阵 RQ 双向追踪：验证/总装成员职责 → NOT_RUN。
3. 论证包（三路线比较/TRL/WBS/课题包/独立否定意见）：论证成员职责 → NOT_RUN。
4. `outputs/FINAL_ACCEPTANCE.md` 与严重问题清零：总装职责 → NOT_RUN。

## 8. 已知差异与阻塞（上报平台裁定）

- **DISC-001**: acceptance.json 硬门 `three_speed_scenarios_present` 与本轮批准范围（单一 0.03c）冲突；CCR-001 未获平台确认前该硬门按字面无法通过。本节点不擅自扩基线、不改验收准则。
- **DISC-002**: 任务书 §4.3 三情景比较受同一约束；应用仅实现 0.03c 并预留接口（`compareScenariosStub`）。
- 其余成员依赖：需求验证矩阵、论证包、FINAL_ACCEPTANCE 由对应成员完成；本节点已提供 `member_acceptance.json` 供总装引用。
