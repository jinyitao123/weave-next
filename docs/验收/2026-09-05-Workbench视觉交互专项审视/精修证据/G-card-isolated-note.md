# 会话卡片隔离验收

全部 G- 截图使用明确标注的隔离测试数据，非真实运行。独立临时 Vite 直接加载当前生产 `WorkTaskConversationCard`、当前源码 primitives 和主题。只检查前端收到给定投影之后的呈现，不验证、不声称后端真实状态过渡。

最终结果通过。

- 1440 与 390 视口分别检查 preparing、running、waiting、failed、completed 有最终成果、completed 无最终成果、stopped，共 14 个静态组合。静态样本卡片实际宽度分别为 524 / 294 px。页面和卡片均无横向溢出；状态与主入口可读，有最终成果时成果入口优先，无最终成果时明确表示待核实。
- running 有活动点及脉冲；waiting、failed、completed、stopped 没有活动脉冲。preparing 创建样本呈现创建说明和步骤披露，本身没有活动圆点。
- 同一个挂载卡片切换 running → waiting → completed，DOM 卡片身份保持不变；活动标记与其动画在 waiting / completed 停止。普通模式下个别按钮仍有短暂颜色过渡，它们不属于活动脉冲。
- reduced-motion 模式下，running 状态点仍可见，脉冲和卡片子树动画均停止；同时复查其余七个静态样本没有运行中的动画。

本轮发现并闭环的问题：最初 waiting 与 completed 无最终成果的状态圆点透明，原因是样式引用了主题未定义的 `state-warning-primary`。主任务修正为已定义的 `state-warn-primary` 后重新加载隔离服务复验，两种状态在两种宽度均呈现 `rgb(245, 158, 11)`，最终截图和测量已更新。此验收子任务没有修改生产文件。

证据为 `G-card-states-{1440|390}.png`、`G-card-reduced-motion-{1440|390}.png` 与 `G-card-isolated-results.json`。

复现命令（仓库根目录）：

```sh
node 'docs/验收/2026-09-05-Workbench视觉交互专项审视/精修证据/card-isolated-evidence.mjs'
```

脚本保留用于复现；临时入口、样本和缓存已删除，临时服务及 Chromium 已关闭。未访问用户标签页，未覆盖共享构建。
