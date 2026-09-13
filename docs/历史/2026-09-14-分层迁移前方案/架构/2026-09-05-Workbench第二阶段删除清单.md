# Workbench 第二阶段删除清单

2026-09-05 · 依据[并仓迁移计划](2026-09-05-Workbench并仓迁移计划.md)第二阶段生成

本清单不凭目录名称判断去留，而是从 Workbench 的实际运行闭包出发。闭包生成方式：

```sh
cd workbench
pnpm --filter "@deepseek-ai/dsh..." --silent exec pwd   # Workbench 入口及其全部工作区依赖
pnpm -r --silent exec pwd                                # 全部工作区项目
```

结果：工作区共 226 个项目，200 个在 `@deepseek-ai/dsh`（Workbench 入口）闭包内，26 个在闭包外。每批删除后都必须重算闭包并跑完整验证，不能只删不测。

## A 类：直接删除（独立 DSH 产品面与发布能力）

| 路径 | 理由 |
|---|---|
| `workbench/website/` | 通用网站，非 Workbench 产品面 |
| `workbench/python/` 与 `workbench/packages/code-runtime/code-runtime-python/` | Python SDK 与单 exe 发布，独立发布入口 |
| `workbench/packages/e2b/`（e2b、fs-e2b、subprocess-e2b） | E2B 云沙箱，独立产品面 |
| `workbench/packages/lsp/`（lsp、lsp-stdio、tool-lsp） | LSP 编辑器集成，Workbench 不使用 |
| `workbench/packages/sdk/client/` | 独立 SDK 客户端（sdk/protocol、sdk/server 在闭包内，保留） |
| `workbench/packages/subagent/`（subagent-acp、subagent-claude-code、subagent-codex、subagent-dsh-sdk） | DSH 自带的引擎子代理驱动；Weave 的 Codex/Claude 引擎是 weave 镜像内的独立 CLI，不经过这些包 |
| `workbench/packages/experimental/`（inspector、agent-team-web-profile、client-ui-agent-team） | 实验面板，不在闭包 |
| `workbench/packages/web/web-search-perplexity/` | 未接入的搜索供应商 |
| `workbench/packages/session/session-title-all-prompts-llm/` | 未使用的标题策略 |
| `workbench/packages/session-query/tool-session-query/` | 未挂接的工具 |
| `workbench/packages/storage/storage-sqlite/` | 未使用的存储后端 |
| `workbench/packages/terminal/tool-terminal/` | 未挂接的工具 |
| `workbench/scripts/` 中的发布机器：build-exe-for-python-sdk\*、build-python-release.py、publish-npm-baseline.ts、release/ | 独立 DSH 发布能力 |
| `workbench/pytest.ini`、`workbench/BENCHMARK.md` | 随 Python/基准发布面退役 |

## B 类：验证后删除（闭包外但有牵连，先查引用）

| 路径 | 牵连 | 处置 |
|---|---|---|
| `workbench/.github/`（19 个 workflow 与 issue 机器人） | `landlock-run.yml` 是唯一仍在保护保留资产的检查 | 先把 landlock-run 原生构建检查移植进根 `go-ci.yml`（或让根 CI 直接构建两张镜像），再整体删除 `.github/`。`landlock-run-release.yml` 属独立发布，不移植 |
| `workbench/packages/test-support/session-snapshot/`、`workbench/snapshots/`、`vitest.snapshot.config.ts`、`scripts/session-snapshot-corpus.corpus.ts` | DSH 快照测试体系；`packages/sandbox/sandbox-local` 有疑似文本引用，先确认 | 确认引用仅为同名文本后一并删除 |
| `workbench/packages/typert/generator/` | 被根 `package.json` 与若干 scripts 引用，属构建期 codegen | 不在首批；与 scripts 工具链清理一并评估 |
| `vitest.web-stress.config.ts`、`vitest.web.perf.config.ts`、`vitest.expected.config.ts`、`scripts/check-expected-filenames.sh` | 上游 Web 压测/文件名治理 | `make workbench-check` 不依赖，确认后删除 |
| `workbench/lefthook.yml` | git hooks，绑定旧仓库根 | 删除；如需要 hooks 在根仓库重建 |
| `workbench/BRAND_GUIDELINES.*`、`SAFETY.*`、`CONTRIBUTING.*` | DSH 品牌与社区入口 | 删除，现行结论归并到根 `docs/` 与根 README |
| `workbench/README*`、`workbench/docs/` 中的产品文档 | 与根 `docs/` 重复 | 工程目录类文档（config-catalog、persistence-catalog、subsystems）保留；产品叙述去重后删除 |

## C 类：禁止误删（名字像示例/实验，实际在运行闭包内）

- `workbench/packages/examples/agent-spine-demo/` — 在闭包内，删除会破坏安装。
- `workbench/packages/experimental/`（agent-team、agent-team-profile、tool-agent-team、webworker-packer、webworker-runtime）— 在闭包内。
- `workbench/vendor/`、`workbench/patches/` — pnpm overrides 与 patchedDependencies 的实际来源。
- `workbench/native/landlock-run/` 根目录 — 不在 dsh 闭包，但根 `Dockerfile.workbench` 以 `pnpm --dir native/landlock-run build:native` 构建它，必须保留；其 `packages/*` 在闭包内。
- `workbench/deploy/workbench.nginx.conf` — 根 `docker-compose.platform.yml` 的网关服务直接引用。
- `workbench/knip.json` — 保留为运行闭包/死代码检查工具。

## 执行顺序

每批一个提交，保持主分支可构建：

1. A 类整批删除 → `pnpm install` 重新生成锁文件并随批提交。
2. B 类逐项确认引用后删除（`.github/` 须先完成 CI 移植）。
3. 文档去重（README/品牌/贡献入口）。
4. 每批完成后运行：

```sh
make workbench-install
make workbench-check
make compose-check
cd workbench && pnpm exec knip   # 确认无新增死引用
docker build -f Dockerfile.workbench -t weave-workbench .   # 最终批
```

## 通过条件（对齐迁移计划第二阶段）

- 保留目录全部有 Workbench 用途，`pnpm --filter "@deepseek-ai/dsh..."` 闭包不出现缺失包。
- 树内不再有 DSH 独立发布入口（npm/Python/exe/网站流水线）。
- 干净检出上 `make ci && make workbench-check && make compose-check` 通过，两张镜像可构建。
