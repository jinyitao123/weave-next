# 迁移后文档引用与双语配对修复

2026-09-06 修复 Workbench Markdown 中指向已移除包、独立网站、Python SDK 和上游 CI 文件的引用。仅修改作者维护的文档及双语记录；生成目录、检查脚本与注册表由对应执行者维护。

当前文档删除不存在的包目录项、相关阅读项及其推荐语句。文件系统文档保留本地与沙箱后端；标题文档保留首消息策略；终端服务和后端文档不再把已移除的 tool-terminal 写成随附消费方。SDK 文档保留实际存在的协议和服务端，Python SDK 教程改为当前可用范围与根产品入口。LSP 页面去掉不存在的类型与服务定义，保留历史入链锚点。

历史决策保留原事实和路径文本，并明确其属于原运行底座的历史组件。实际移到根目录的 Dockerfile 和 CI 引用指向当前文件。归档 Agent Notes 未修改，未恢复删除组件或增加失效链接例外。

中英文同步维护，当前编辑涉及的 63 组文档用原 pairing 工具记录。根 Workbench README 补齐语言切换，并同时链接根产品英文和中文说明。生成的能力关系图与事件表中文页按最新英文机器字段同步，保留经核对的中文说明。

实测检查结果如下，均从 `workbench/` 执行，退出码为 0。

| 命令 | 结果 |
| --- | --- |
| `node node_modules/tsx/dist/cli.mjs scripts/verify-md-links.ts` | 2137 文件，相对链接和锚点全部有效 |
| `node node_modules/tsx/dist/cli.mjs scripts/verify-package-paths.ts` | 4422 文件，包路径引用全部有效 |
| `node node_modules/tsx/dist/cli.mjs scripts/verify-translation-pairing.ts` | 1063 组双语文档全部一致 |
| `node node_modules/tsx/dist/cli.mjs scripts/verify-agent-note-format.ts` | 654 条 Agent Notes 格式通过 |
| `node node_modules/tsx/dist/cli.mjs scripts/verify-md-wrap.ts` | 2143 文件，无硬换行段落 |
| `git diff --check` | 无空白错误 |

这些结果表示链接、路径、配对与格式检查通过，不代替其他执行者负责的完整 doc-sync、代码验证或最终浏览器验收。
