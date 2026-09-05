# doc-sync 失败归因与本次生成物同步

复验日期为 2026-09-06，比较基线为 `c5abcc5421ccc858c12501636ba1a9879891bcfc`，检查对象为当前工作区。原始 `doc-sync` 结果为 17 项通过、15 项失败。本次定位到三个含有当前改动增量的生成物，均已使用原生成器完整同步并通过各自 `--check`。其余失败有 HEAD 基线证据，不计为本次交互整改新增问题，也不宣称全量 `doc-sync` 已通过。

## 归因方法

- 将 `git archive HEAD:workbench` 导出到临时目录，共用当前依赖安装；源文件来自 HEAD，未把工作区改动复制进基线。
- 比较相关检查脚本和 `package.json`，均与 HEAD 一致。对目录、注册表和相对路径类失败，通过 HEAD 重新运行或 `git cat-file` 核对缺失目标。
- 对 client、config、persistence、tool catalog，分别比较 HEAD 源码与工作区源码的生成结果，区分“基线已经失败”和“本次仍有生成物增量”，避免仅凭两边退出码相同就认定与本次无关。
- 临时导出目录的父级缺少仓库根 README，导致 markdown 检查额外出现两条路径失败；当前工作区的 355 条失败均在 HEAD 的 357 条中。额外两条属于验证目录布局差异，未计入项目问题。

## 原始 15 项失败

| 检查 | 原始失败及 HEAD 证据 | 本次处理 |
| --- | --- | --- |
| documentation build | `pnpm run docs:build` 对应脚本不存在；HEAD 的 `package.json` 同样不存在该脚本 | 迁移基线，未改 gate |
| type equivalence | `packages/lsp/lsp/src/types.ts` 不存在；HEAD 与当前均缺失，checker 未改 | 迁移基线 |
| doc graphs | `e2b, inspector, lsp` 的 service role classification 过期；HEAD 复现相同失败 | 迁移基线 |
| markdown links | 355 条相对路径失败；全部在 HEAD 重跑结果中复现，失败条目不涉及本次修改文档 | 迁移基线 |
| cordis catalog | 三个 service partition 违规，仍引用 `e2b, inspector, lsp`；HEAD 复现 | 迁移基线 |
| translation pairing | 40 条失败；与 HEAD 的失败条目完全一致 | 本次涉及的 9 组文档单独检查全部通过 |
| client catalog | HEAD 已因删除 `experimental-client-ui-agent-team` 后 occupant 未同步而失败；本次增加日志导出条件注册的一行生成差异 | 完整生成并通过 `--check` |
| tool catalog | HEAD 与当前都 stale；两边纯生成结果均 1829 行且逐字一致 | 迁移基线，保留原文件 |
| config catalog | HEAD 已 stale；两边纯生成结果仅差一个 Workbench Config Source 行号 | 完整生成并通过 `--check`；同时清理旧 owner 的自动索引 |
| persistence catalog | HEAD 的 `--check` 通过；本次仅两个 Source 行号移动 | 完整生成并通过 `--check` |
| package paths | 两条旧 note 仍指向已移除的 `subagent-dsh-sdk/tests/fixtures/loader`；HEAD 复现两条相同失败 | 迁移基线 |
| package README model experience | 10 条 allowlist 仍指向已移除的 package README；HEAD 复现相同十条 | 迁移基线 |
| archived agent notes | checker 查询 `HEAD:.agents/notes/archived/manifest.json`，实际受版本控制路径在 `HEAD:workbench/.agents/...`；HEAD 路径事实与当前一致 | 迁移后的 Git 根路径假设未同步 |
| documentation standard tests | library registry 仍列出已移除的 `packages/code-runtime/code-runtime-python/src/index.ts`；HEAD 与当前均缺失，checker 和 registry 未改 | 迁移基线 |
| documentation site checks | gate 指定的 `scripts/project-doc-site.spec.ts`、`scripts/verify-doc-site-fragments.spec.ts` 在 HEAD 与当前均不存在 | 迁移基线 |

## 三个生成物的实际变更

`workbench/docs/persistence-catalog.md` 仅更新 `weave/work-task` 与 `weave/work-task-action` 的 Source 行号，分别为 `224 → 228`、`226 → 230`。同一生成器同时校验的 `packages/core/session/src/known-event-types.ts` 无内容变化。

`workbench/packages/extensions/cordis-client-runner/src/client/slot-catalog.ts` 为 1 行新增、2 行删除。删除已不存在的 `experimental-client-ui-agent-team` occupant；日志导出 occupant 更新为生成器对 Workbench 条件组件选择的词法描述。没有手工更改生成描述，也没有修改生成器。

`workbench/docs/config-catalog.md` 为 3 行新增、463 行删除。完整生成的较大差异来自之前的迁移后索引陈旧。

- 删除 14 个已经不存在的 owner 的 Config 段，涉及 `e2b`、`experimental-inspector`、`lsp-stdio`、`session-title-all-prompts-llm`、`storage-sqlite`、`subagent-acp`、`subagent-claude-code`、`subagent-codex`、`subagent-dsh-sdk`、`subprocess-e2b`、`tool-lsp`、`tool-session-query`、`tool-terminal`、`web-search-perplexity`。
- 从无配置或 library 列表删除另外 6 个已移除 package 条目，涉及 `experimental-client-ui-agent-team`、`fs-e2b`、`lsp`、`code-runtime-python`、`experimental-agent-team-web-profile`、`sdk-client`。上述总计 20 个入口均经 `git cat-file` 与文件系统核对，HEAD 和当前均不存在。
- 补入 `runtimeServerUrl` 字段及其一行注释。该字段已存在于 HEAD 的 Workbench Config，属于既有文档遗漏。
- Source 从提交中的 `817` 更新到当前源码 `905`。HEAD 源码的真实 Config 行号为 `829`；本次源码造成的增量为 `829 → 905`。

## 本次范围检查

以下命令均从 `workbench/` 执行，退出码为 0。

```sh
node node_modules/tsx/dist/cli.mjs scripts/gen-client-catalog.ts --check
node node_modules/tsx/dist/cli.mjs scripts/gen-config-catalog.ts --check
node node_modules/tsx/dist/cli.mjs scripts/gen-persistence-catalog.ts --check
node node_modules/tsx/dist/cli.mjs scripts/verify-translation-pairing.ts \
  packages/bundle/workbench-app/README.md \
  packages/client/ui-chat/README.md \
  packages/client/ui-conversation/README.md \
  packages/client/ui-layout/README.md \
  packages/client/ui-model-selection/README.md \
  packages/client/ui-weave/README.md \
  packages/client/ui-workspace/README.md \
  packages/session-query/session-log-export/README.md \
  .agents/notes/implemented/bug-fix/2026-09-05-workbench-task-state-and-reading-continuity.md
```

最后一条输出为 `9 named pair(s) consistent`。检查包含每组的英文、中文和 `.i18n.yaml` 一致性记录。三个 catalog 均先运行对应生成命令，再执行上述 `--check`，未手工修补行号。没有修改 checker、gate 或已有翻译配对规则，也没有为消除旧失败恢复已移除的独立产品入口。同步后未扩大运行全量 doc-sync，因此不将剩余项的预期结果表述为新的全量运行结果。

## 2026-09-06 继续修复迁移遗留问题

用户随后授权修复剩余问题，本轮基线更新为 `8e201fc`。以上记录保留原轮结论；本节记录新增处理范围和验证证据。

### 退役网站的检查归属

[第二阶段删除清单](../../../架构/2026-09-05-Workbench第二阶段删除清单.md) 的 A 类条目明确将 `workbench/website/` 归为“通用网站，非 Workbench 产品面”。当前 checkout 中该网站、`docs:build`/`docs:build:mpa` 脚本，以及 `project-doc-site.spec.ts`、`verify-doc-site-fragments.spec.ts` 均不存在。因此从 `run-gates.ts` 删除其构建与投影测试叶子，并同步 Windows 模式中相同旧网站的依赖关系；没有删掉现存文档的链接、类型、格式、双语、目录新鲜度或归档检查。`doc-sync` 从 32 个叶子收敛为 30 个实际存在的检查。

### 保留严格检查的证据

- 归档检查继续读取 Git 基线并比较每个封存哈希，仅将 `ref:path` 修正为相对工作区的 `ref:./path`。实际工作区通过 507 个冻结归档校验，归档内容及封存清单没有修改。
- 新增回归在临时 Git 仓库内建立嵌套 `workbench/`，提交合法归档基线，运行真实 `verify-archived-agent-notes.ts` 得到退出码 0；随后删除工作区清单中的封存项，检查以退出码 1 和 `sealed manifest entry is missing` 拒绝。它证明路径修复没有绕过基线保护。
- `run-gates.spec.ts` 与 `archived-agent-notes.spec.ts` 共 71 项测试通过；`doc-standard.spec.ts`、`gen-doc-graphs.spec.ts` 与 `gen-cordis-catalog-partition.spec.ts` 的 25 项测试通过。后者仍包含漏掉声明、错误分区等非法输入的拒绝用例。
- 本轮修改的 8 个 TypeScript 脚本执行局部 `pnpm exec oxlint`，退出码为 0；`git diff --check -- workbench/scripts` 通过。
- 清单清理仅删除文件系统中已不存在的服务、库、README 例外和 LSP 类型源；现存服务完整性、类型链接覆盖和生成物新鲜度规则保持执行。新 `SessionHistoryProjection` 登记为有明确 `session-controller/README.md` 归属的服务注册参数，未通过基础类型列表隐藏它。

### 最终复验

主线程源码及双语文档稳定后，重新生成 config、persistence、Cordis、Cordis inspect、client、tool、module graph 和文档图表，并同步中文内容。最终从 `workbench/` 执行 `pnpm run doc-sync`，退出码为 0，结果为 `30 passed, 0 failed, 0 skipped in 24.03s`。其中包括真实 Host 构建后的 doc-typecheck、服务类型链接覆盖、全部目录新鲜度、Markdown 相对链接、双语配对、README 内容要求和冻结归档检查。

最后一次失败涉及终端包清理旧工具说明后缺少 Model Experience 的代码锚定，已补入现存 `ctx.terminals`、`registerBackend`、`spawn(owner, request)` 的间接贡献说明，中英文同步；全量 230 个 README 通过。历史机制旧 note 也已链接新投影历史决策，明确保留仍生效的 journal、canonical baseline 与 Gateway 规则。

构建期间发现新增 e2e 被误归入 Client 编译项目，产生 800 个未跟踪的 `src/` 同名编译文件。主线程修正 Host include 与 Client exclude 后，按清单仅清理这些有对应 `.ts`/`.tsx` 源文件的 `.js`、`.js.map`、`.d.ts`、`.d.ts.map` 产物；检查确认残留为 0、`workbench/vendor` 无工作区改动。没有通过放宽源码扫描规则来隐藏这次污染。文档与检查脚本的 `git diff --check` 通过。
