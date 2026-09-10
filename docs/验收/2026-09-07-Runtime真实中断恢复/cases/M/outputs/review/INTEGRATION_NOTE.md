# 上游成果整合说明（INTEGRATION NOTE）

- **追溯键**: baseline_id = `corona-baseline-1.0.0` · baseline_version = `1.0.0` · content_digest = `cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2`
- **节点**: luna-digital-engineering-builder（模型 / 图纸 / 应用 / 成员级验证清单）
- **日期**: 2026-09-07

按 ICD-DLV-001，本节点通过平台传递的上游输入接力，将上游当前版本**字节相同复制**到本节点 `outputs/` 后整合交付；未扫描同机其他成员工作目录，未使用任何旧运行文件。[verified_fact]

## 复制的上游文件与摘要核验（本轮实际执行 `shasum -a 256`）

| 本节点路径 | 上游来源（inputs/） | SHA-256 | 核验 |
|---|---|---|---|
| `outputs/model/baseline_frozen.yaml` | `inputs/lead/model/baseline_frozen.yaml` | `cfb12b78…8b10c2` | 与 inputs/manifest.json 一致 [verified_fact] |
| `outputs/review/change_control_register.md` | `inputs/lead/review/change_control_register.md` | `2494de35…87915c` | 与 manifest 一致 [verified_fact] |
| `outputs/review/interface_control.md` | `inputs/lead/review/interface_control.md` | `bded6d00…2b5f76` | 与 manifest 一致 [verified_fact] |
| `outputs/review/requirements_baseline.md` | `inputs/lead/review/requirements_baseline.md` | `53c628a1…ed4481` | 与 manifest 一致 [verified_fact] |

## 派生物

- `outputs/model/params.json` — 由 `outputs/model/baseline_frozen.yaml` 经 ruby `YAML.load_file` + `JSON.pretty_generate` 生成的机器可读副本（系统 Python 无 PyYAML，ICD-SOT-002 允许使用机器可读副本）；内容一致性由 `run_tests.py` 中摘要比对与参数抽查保障。[derived_result]

## 范围声明

- 论证包正文（三路线比较、登记册、TRL/风险/成本/WBS、课题包、独立否定意见）属论证成员职责，不在本节点；本目录仅含上游接力副本与本说明。[verified_fact]
- `outputs/FINAL_ACCEPTANCE.md` 属最终总装成员职责；本节点交付成员级验证清单 `outputs/verification/member_acceptance.json` 供其引用。最终验收中必须显式披露 DISC-001/002。[verified_fact]
