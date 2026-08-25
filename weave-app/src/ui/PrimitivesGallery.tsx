import { Button } from "./Button";
import { Badge } from "./Badge";
import { Card } from "./Card";
import { Field } from "./Field";

/**
 * 原语陈列页：仅供人工核对四组件的变体与状态。
 * 不挂路由、不被任何页面引用，因此不会进入打包产物。
 * 核对方式：在任意页面临时 `import { PrimitivesGallery } from "../ui/PrimitivesGallery"` 渲染。
 */
export function PrimitivesGallery() {
  return (
    <div className="ui-gallery">
      <section>
        <h2>Button</h2>
        <div className="ui-gallery__row">
          <Button variant="primary">主要操作</Button>
          <Button variant="secondary">次要操作</Button>
          <Button variant="danger">危险操作</Button>
          <Button variant="ghost">幽灵按钮</Button>
          <Button variant="primary" loading>提交中</Button>
          <Button variant="secondary" disabled>不可用</Button>
          <Button variant="primary" size="small">小号按钮</Button>
        </div>
      </section>
      <section>
        <h2>Badge</h2>
        <div className="ui-gallery__row">
          <Badge>默认</Badge>
          <Badge tone="accent">进行中</Badge>
          <Badge tone="success">已完成</Badge>
          <Badge tone="warning">待处理</Badge>
          <Badge tone="danger">失败</Badge>
        </div>
      </section>
      <section>
        <h2>Card</h2>
        <div className="ui-gallery__row">
          <Card header={<strong>标准卡片</strong>} footer={<Badge tone="accent">状态</Badge>}>
            <p>卡片正文，header/footer 用分隔线分区，不再嵌套卡片。</p>
          </Card>
          <Card padding="compact" interactive header={<strong>紧凑可交互</strong>}>
            <p>hover 抬升，键盘可聚焦。</p>
          </Card>
        </div>
      </section>
      <section>
        <h2>Field</h2>
        <Field label="项目名称" help="2-40 个字符">
          {({ id, ...control }) => <input id={id} {...control} type="text" placeholder="输入项目名称" />}
        </Field>
        <Field label="任务描述" error="任务描述不能为空">
          {({ id, ...control }) => <textarea id={id} {...control} rows={3} />}
        </Field>
      </section>
    </div>
  );
}
