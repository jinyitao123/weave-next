/** Reading controls merged into the Weave locale namespace by its owner. */
export const zh = {
  'task.preview.table': '表格预览',
  'task.preview.tableLimited': '当前展示前 200 行、前 40 列，完整内容请下载文件。',
  'task.preview.zoomIn': '放大图纸',
  'task.preview.zoomOut': '缩小图纸',
  'task.preview.zoomReset': '适应宽度',
  'task.preview.imageLoading': '正在读取图纸',
  'task.preview.imageUnavailable': '图纸暂时无法预览，请尝试下载完整文件。',
} satisfies Record<string, string>

/** English reading controls. */
export const en = {
  'task.preview.table': 'Table preview',
  'task.preview.tableLimited': 'Showing the first 200 rows and 40 columns. Download the file for the complete contents.',
  'task.preview.zoomIn': 'Zoom in',
  'task.preview.zoomOut': 'Zoom out',
  'task.preview.zoomReset': 'Fit width',
  'task.preview.imageLoading': 'Loading drawing',
  'task.preview.imageUnavailable': 'This drawing cannot be previewed right now. Try downloading the complete file.',
} satisfies Record<keyof typeof zh, string>

/** Localized copy required by the reading component. */
export interface DeliverablePreviewLabels {
  table: string
  tableLimited: string
  zoomIn: string
  zoomOut: string
  zoomReset: string
  imageLoading: string
  imageUnavailable: string
}

/**
 * Resolve reading controls through the owning dictionary.
 * @param t - The bound Weave translator.
 * @returns Localized reading controls.
 */
export function deliverablePreviewLabels(t: (key: keyof typeof zh) => string): DeliverablePreviewLabels {
  return {
    table: t('task.preview.table'), tableLimited: t('task.preview.tableLimited'),
    zoomIn: t('task.preview.zoomIn'), zoomOut: t('task.preview.zoomOut'), zoomReset: t('task.preview.zoomReset'),
    imageLoading: t('task.preview.imageLoading'), imageUnavailable: t('task.preview.imageUnavailable'),
  }
}
