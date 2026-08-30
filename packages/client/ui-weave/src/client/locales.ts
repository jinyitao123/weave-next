/** Dictionary namespace owned by the Weave presentation plugin. */
export const NS = 'weave'

/** Simplified Chinese dictionary and key-set source of truth. */
export const zh = {
  'teamList.title': '团队匹配',
  'teamList.running': '正在读取可用团队',
  'teamList.failed': '读取团队失败',
  'teamList.stopped': '读取团队已中止',
  'teamList.found': '发现 {count} 个候选团队',
  'teamList.empty': '当前没有可用团队',
  'teamList.invalid': '团队数据无法识别',
  'teamList.dispatchable': '可通过默认工作流派发',
  'teamList.noWorkflow': '缺少默认工作流',
  'teamList.archived': '已归档',
  'teamList.building': '正在组建',
  'teamList.needsRepair': '需要修复',
  'teamList.scenario': '适用场景',
  'teamList.success': '完成标准',
  'teamList.responsibilities': '可承担',
  'deliverable.title': '最终交付物',
  'deliverable.running': '正在读取交付物',
  'deliverable.ready': '文件已就绪',
  'deliverable.failed': '读取交付物失败',
  'deliverable.stopped': '读取交付物已中止',
  'deliverable.invalid': '交付物数据无法识别',
  'deliverable.download': '下载文件',
  'deliverable.preview': '文件内容',
} satisfies Record<string, string>

/** Locale key union for Weave product presentations. */
export type WeaveKey = keyof typeof zh

/** English dictionary checked against the Chinese key set. */
export const en = {
  'teamList.title': 'Team match',
  'teamList.running': 'Reading available teams',
  'teamList.failed': 'Could not read teams',
  'teamList.stopped': 'Team lookup stopped',
  'teamList.found': 'Found {count} candidate teams',
  'teamList.empty': 'No teams are currently available',
  'teamList.invalid': 'Team data could not be read',
  'teamList.dispatchable': 'Ready for default-workflow dispatch',
  'teamList.noWorkflow': 'No default workflow',
  'teamList.archived': 'Archived',
  'teamList.building': 'Building',
  'teamList.needsRepair': 'Needs repair',
  'teamList.scenario': 'Best for',
  'teamList.success': 'Done when',
  'teamList.responsibilities': 'Can handle',
  'deliverable.title': 'Final deliverable',
  'deliverable.running': 'Reading deliverable',
  'deliverable.ready': 'File ready',
  'deliverable.failed': 'Could not read deliverable',
  'deliverable.stopped': 'Deliverable lookup stopped',
  'deliverable.invalid': 'Deliverable data could not be read',
  'deliverable.download': 'Download file',
  'deliverable.preview': 'File contents',
} satisfies Record<WeaveKey, string>
