/** Locale namespace for the read-only project activity view. */
export const NS = 'projectActivity'

/** Chinese project activity copy. */
export const zh = {
  title: '项目进展 · {name}', close: '关闭', empty: '这个项目还没有关联的团队任务。',
  unavailable: '这个工作区已不可用。', incomplete: '部分任务记录尚未加载，当前汇总不完整。',
  scope: '仅显示这个工作区关联的任务；处理问题与领取成果会回到原任务。',
  tasks: '{n} 个任务', teams: '{n} 个团队', attention: '{n} 项待办', outputs: '{n} 份可用成果',
  team: '团队', teamUnknown: '未标明团队', members: '成员', membersUnknown: '成员记录尚未提供',
  activity: '最近活动', noActivity: '尚未记录阶段进展', location: '运行位置', locationUnknown: '运行位置尚未提供',
  todo: '待办', noTodo: '当前没有需要你处理的事项', openTask: '打开工作对话', openResults: '查看成果',
  saved: '已保存成果', noOutputs: '尚无已保存成果', stage: '阶段记录', final: '最终成果', summary: '交付结论',
  unreviewed: '成果是否合格仍需确认', stale: '进展尚未更新，请打开原任务查看当前状态',
  human: '回答团队的问题', correction: '确认修改范围', failed: '查看原因并处理',
  missingOutput: '执行已结束，最终成果待核实', review: '查看成果并确认是否采用', revision: '讨论需要修改的内容',
  preparing: '准备中', queued: '排队中', running: '执行中', waiting: '等待继续', stopping: '正在停止',
  completed: '已产生成果', stopped: '已停止', failedStatus: '执行中断', stopUnconfirmed: '停止状态未确认',
  memberPending: '等待开始', memberRunning: '进行中', memberPartial: '部分完成', memberCompleted: '本阶段完成',
  memberFailed: '遇到问题', memberStopped: '已停止', memberUnknown: '状态未记录',
  updated: '记录时间 {time}（UTC）', neverUpdated: '尚未取得运行进展', progress: '{done}/{total} 个阶段',
} satisfies Record<string, string>

/** Keys owned by the project activity view. */
export type ProjectActivityKey = keyof typeof zh

/** English project activity copy. */
export const en = {
  title: 'Project activity · {name}', close: 'Close', empty: 'No team tasks are associated with this project yet.',
  unavailable: 'This workspace is no longer available.', incomplete: 'Some task records have not loaded. This summary is incomplete.',
  scope: 'Only tasks associated with this workspace are shown. Actions and results open their original task.',
  tasks: '{n} tasks', teams: '{n} teams', attention: '{n} items to address', outputs: '{n} saved outputs',
  team: 'Team', teamUnknown: 'Team not identified', members: 'Members', membersUnknown: 'Member records are not available yet',
  activity: 'Latest activity', noActivity: 'No stage progress recorded yet', location: 'Execution location', locationUnknown: 'Execution location not available yet',
  todo: 'To do', noTodo: 'Nothing currently requires your response', openTask: 'Open work conversation', openResults: 'View outputs',
  saved: 'Saved outputs', noOutputs: 'No saved outputs yet', stage: 'Stage record', final: 'Final output', summary: 'Delivery conclusion',
  unreviewed: 'Output quality still needs confirmation', stale: 'Progress has not updated. Open the original task for its current state',
  human: 'Answer the team’s question', correction: 'Confirm the scope of changes', failed: 'Review the cause and next action',
  missingOutput: 'Execution ended; final delivery is unconfirmed', review: 'Review the outputs and decide whether to adopt them', revision: 'Discuss the changes needed',
  preparing: 'Preparing', queued: 'Queued', running: 'Running', waiting: 'Waiting to continue', stopping: 'Stopping',
  completed: 'Outputs recorded', stopped: 'Stopped', failedStatus: 'Execution interrupted', stopUnconfirmed: 'Stop unconfirmed',
  memberPending: 'Not started', memberRunning: 'In progress', memberPartial: 'Partly completed', memberCompleted: 'Stage completed',
  memberFailed: 'Needs attention', memberStopped: 'Stopped', memberUnknown: 'State not recorded',
  updated: 'Recorded at {time} (UTC)', neverUpdated: 'No run progress received yet', progress: '{done}/{total} stages',
} satisfies Record<ProjectActivityKey, string>
