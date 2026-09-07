# 日冕计划 · 数字决策应用 (outputs/app/)

- **追溯键**: baseline_id = `corona-baseline-1.0.0` · baseline_version = `1.0.0` · content_digest = `cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2`
- **等级**: 概念级 (Level-0 / Pre-Phase A) [verified_fact]
- **情景**: 单一 0.03c 锁定；三速度为 CCR-001 待确认变更，仅预留接口不实现 (DISC-002) [verified_fact]

## 运行（离线，无云服务）

直接用浏览器打开 `index.html`（file:// 协议即可）。无外部资源、无网络请求、无需服务器。

## 功能

- 可调：质量（对数滑杆 1–1e10 kg）、推进效率 η、人工重力半径、转速；巡航速度 0.03c 锁定。
- 实时显示：航行时间、动能下限（非相对论+相对论对照分列）、等效能量、最小输入能量下限、人工重力、尘埃撞击能量。
- 三路线证据状态 / 成立条件 / 终止条件 / 主要未知项面板（可分离，禁止外推）。
- 概念图纸链接（`../drawings/` 四个文件）。
- 「导出当前情景 JSON」：含追溯键三元组与全部输入/输出（浏览器 Blob 下载）。

## 文件

| 文件 | 说明 |
|---|---|
| `index.html` | 应用入口（app_entry） |
| `params.js` | 由 `../model/parameters.json` 受控生成，含追溯键 |
| `core.js` | 计算内核，与 `../model/corona_model.py` 同公式同参数 (RQ-APP-006) |
| `app.js` | UI 逻辑 |
| `check_app.js` | 有界机器检查：`node check_app.js`（自行结束，exit 0=PASS） |
| `sample_export_0.03c.json` | check_app.js 生成的导出样例（与浏览器导出同构） |

## 机器检查

```bash
cd outputs/app && node check_app.js
```

比对应用内核与 `../model/output/reference_values.json` 实际模型输出（1e-12 容差），验证离线性、图纸链接、导出结构与事实标签。
