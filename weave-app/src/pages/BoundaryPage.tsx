import type { LucideIcon } from "lucide-react";
import { Cable, CheckCircle2 } from "lucide-react";

interface BoundaryPageProps {
  eyebrow: string;
  title: string;
  description: string;
  icon: LucideIcon;
  available: string[];
  missing: string[];
}

export function BoundaryPage({ eyebrow, title, description, icon: Icon, available, missing }: BoundaryPageProps) {
  return (
    <div className="page">
      <header className="page-header"><div><p className="eyebrow">{eyebrow}</p><h1>{title}</h1><p>{description}</p></div></header>
      <div className="boundary-layout">
        <section className="boundary-intro"><Icon size={28} /><h2>页面边界已建立</h2><p>导航、布局与状态边界属于正式前端；业务控件只会在逐字段接入真实 handler 后开放。</p></section>
        <section className="boundary-list"><h2>当前可读取</h2>{available.map((item) => <p key={item}><CheckCircle2 size={16} /> {item}</p>)}</section>
        <section className="boundary-list boundary-list--missing"><h2>仍需合同</h2>{missing.map((item) => <p key={item}><Cable size={16} /> {item}</p>)}</section>
      </div>
    </div>
  );
}
